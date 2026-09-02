package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/singha105/webhook-relay/internal/models"
)

// ClaimDueEvents takes up to limit events that are ready for delivery, marks
// them 'delivering', and stamps a lease.
//
// # Why a lease rather than a transaction held across the enqueue
//
// The obvious outbox implementation opens a transaction, SELECTs FOR UPDATE
// SKIP LOCKED, XADDs each row to the stream, UPDATEs their status, and commits.
// That is atomic-ish and we deliberately do not do it, because it holds a
// Postgres transaction — and its row locks — open across a network round trip
// to a different system. Under the Day 6 load test that turns every Valkey
// hiccup into Postgres lock contention, and a Valkey timeout into a
// long-running transaction that blocks vacuum.
//
// Instead this claims and leases in ONE statement, and the caller enqueues
// afterwards. The row is marked 'delivering' with next_retry_at set to
// now() + lease, which means "if nothing has happened to this by then, assume
// whoever claimed it is gone."
//
// # What happens if the relay dies mid-batch
//
//   - Died after this call, before enqueueing any of the batch: those events
//     sit in 'delivering' with a lease that nobody will renew. When it expires,
//     RequeueExpiredLeases returns them to 'failed' and they are picked up
//     again. Cost: those events are delayed by up to one lease period. No
//     duplicates, no loss.
//   - Died halfway through enqueueing: the enqueued half proceeds normally; the
//     rest is covered by the case above.
//   - Enqueue succeeded but the process died before anything else: the worker
//     picks the event up from the stream as usual. The lease is irrelevant
//     because the worker will move the row to a terminal state or schedule a
//     retry.
//   - Enqueue itself returned an error: the caller immediately releases the
//     claim (see ReleaseClaim) rather than waiting out the lease.
//
// The one thing that cannot happen is losing an event, because the durable row
// is the source of truth and the stream is only a transport. The thing that CAN
// happen is delivering twice, which is the at-least-once contract, and which
// the delivery dedup key and the receiver's own X-Webhook-Id check absorb.
//
// SKIP LOCKED is what lets several relay replicas run concurrently: each takes
// a disjoint set of rows instead of blocking on the same ones.
// DueEvent is a claimed event plus the trace context captured at ingest.
type DueEvent struct {
	ID uuid.UUID
	// TraceContext is the W3C carrier stored when the event was ingested. Nil
	// when the event predates tracing or tracing was disabled, in which case
	// the delivery starts a fresh root rather than failing.
	TraceContext map[string]string
}

func (s *Store) ClaimDueEvents(ctx context.Context, limit int, lease time.Duration) ([]DueEvent, error) {
	const q = `
		WITH due AS (
			SELECT id
			FROM events
			WHERE status IN ('pending', 'failed')
			  AND next_retry_at <= now()
			ORDER BY next_retry_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE events e
		SET status        = 'delivering',
		    next_retry_at = now() + $2::interval
		FROM due
		WHERE e.id = due.id
		RETURNING e.id, e.trace_context`

	rows, err := s.pool.Query(ctx, q, limit, lease.String())
	if err != nil {
		return nil, fmt.Errorf("claim due events: %w", err)
	}
	defer rows.Close()

	out := make([]DueEvent, 0, limit)
	for rows.Next() {
		var (
			id       uuid.UUID
			rawTrace []byte
		)
		if err := rows.Scan(&id, &rawTrace); err != nil {
			return nil, fmt.Errorf("scan claimed event: %w", err)
		}
		due := DueEvent{ID: id}
		if len(rawTrace) > 0 {
			// A corrupt carrier must not stop delivery: the event still goes
			// out, just as the root of its own trace.
			_ = json.Unmarshal(rawTrace, &due.TraceContext)
		}
		out = append(out, due)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim due events: %w", err)
	}
	return out, nil
}

// ReleaseClaim returns an event to the ready set immediately.
//
// Called when the enqueue that was supposed to follow a claim failed. Without
// it the event would wait out the whole lease for no reason — correct, but
// needlessly slow, and it is the common case when Valkey is briefly down.
func (s *Store) ReleaseClaim(ctx context.Context, id uuid.UUID) error {
	const q = `
		UPDATE events
		SET status = 'failed', next_retry_at = now()
		WHERE id = $1 AND status = 'delivering'`

	if _, err := s.pool.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("release claim on %s: %w", id, err)
	}
	return nil
}

// RequeueExpiredLeases returns events whose delivery lease has expired.
//
// This is the safety net for the case the stream cannot cover: a worker that
// died holding an entry AND a Valkey that lost the pending list with it — a
// restarted pod, a flushed database, a recreated consumer group. XAUTOCLAIM
// handles the first failure alone; only a durable lease handles both together.
//
// It is intentionally conservative: it moves the row to 'failed' rather than
// straight back to 'delivering', so the event goes through the normal claim
// path and cannot bypass the attempt budget.
func (s *Store) RequeueExpiredLeases(ctx context.Context, limit int) (int, error) {
	const q = `
		WITH expired AS (
			SELECT id
			FROM events
			WHERE status = 'delivering'
			  AND next_retry_at <= now()
			ORDER BY next_retry_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE events e
		SET status = 'failed', next_retry_at = now()
		FROM expired
		WHERE e.id = expired.id`

	tag, err := s.pool.Exec(ctx, q, limit)
	if err != nil {
		return 0, fmt.Errorf("requeue expired leases: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// DeliveryTarget is everything a worker needs to make one attempt.
type DeliveryTarget struct {
	Event    models.Event
	Endpoint models.Endpoint
}

// LoadForDelivery reads an event and its endpoint in one round trip.
//
// Read at delivery time rather than carried in the queue message, so a
// redelivered or reclaimed message cannot act on a stale copy of a row whose
// URL or secret has since been changed.
func (s *Store) LoadForDelivery(ctx context.Context, eventID uuid.UUID) (*DeliveryTarget, error) {
	const q = `
		SELECT e.id, e.endpoint_id, e.event_type, e.payload, e.status,
		       e.idempotency_key, e.created_at, e.attempt_count,
		       ep.id, ep.url, ep.description, ep.signing_secret, ep.is_active,
		       ep.rate_limit_per_sec, ep.consecutive_failures, ep.created_at, ep.updated_at
		FROM events e
		JOIN endpoints ep ON ep.id = e.endpoint_id
		WHERE e.id = $1`

	var t DeliveryTarget
	var payload []byte
	err := s.pool.QueryRow(ctx, q, eventID).Scan(
		&t.Event.ID, &t.Event.EndpointID, &t.Event.EventType, &payload, &t.Event.Status,
		&t.Event.IdempotencyKey, &t.Event.CreatedAt, &t.Event.AttemptCount,
		&t.Endpoint.ID, &t.Endpoint.URL, &t.Endpoint.Description, &t.Endpoint.SigningSecret,
		&t.Endpoint.IsActive, &t.Endpoint.RateLimitPerSec, &t.Endpoint.ConsecutiveFailures,
		&t.Endpoint.CreatedAt, &t.Endpoint.UpdatedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return nil, fmt.Errorf("load event %s for delivery: %w", eventID, models.ErrNotFound)
		}
		return nil, fmt.Errorf("load event %s for delivery: %w", eventID, err)
	}
	t.Event.Payload = payload
	return &t, nil
}

// MarkDelivered records a successful delivery and resets the endpoint's
// consecutive failure counter.
//
// Both writes happen in one transaction. The counter feeds the Day 3 circuit
// breaker, and a breaker whose reset can be lost independently of the success
// that caused it would eventually open against a perfectly healthy endpoint.
func (s *Store) MarkDelivered(ctx context.Context, eventID, endpointID uuid.UUID, attempt models.DeliveryAttempt) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin mark delivered: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := recordAttemptTx(ctx, tx, attempt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE events SET status = 'delivered', attempt_count = $2 WHERE id = $1`,
		eventID, attempt.AttemptNumber); err != nil {
		return fmt.Errorf("mark event %s delivered: %w", eventID, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE endpoints SET consecutive_failures = 0 WHERE id = $1 AND consecutive_failures <> 0`,
		endpointID); err != nil {
		return fmt.Errorf("reset failure counter for endpoint %s: %w", endpointID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit mark delivered: %w", err)
	}
	return nil
}

// ScheduleRetry records a failed attempt and sets when to try again.
//
// The attempt row, the counter, and the schedule move together. If the attempt
// were recorded outside the transaction, a crash between the two would leave an
// event whose attempt history and attempt_count disagree — and the counter is
// what the budget is enforced against.
func (s *Store) ScheduleRetry(ctx context.Context, eventID, endpointID uuid.UUID, attempt models.DeliveryAttempt, nextRetryAt time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin schedule retry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := recordAttemptTx(ctx, tx, attempt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE events
		 SET status = 'failed', attempt_count = $2, next_retry_at = $3
		 WHERE id = $1`,
		eventID, attempt.AttemptNumber, nextRetryAt); err != nil {
		return fmt.Errorf("schedule retry for event %s: %w", eventID, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE endpoints SET consecutive_failures = consecutive_failures + 1 WHERE id = $1`,
		endpointID); err != nil {
		return fmt.Errorf("increment failure counter for endpoint %s: %w", endpointID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit schedule retry: %w", err)
	}
	return nil
}

// MarkDeadLettered moves an event to the DLQ: retries exhausted, or a permanent
// failure that no number of retries could fix.
func (s *Store) MarkDeadLettered(ctx context.Context, eventID, endpointID uuid.UUID, attempt models.DeliveryAttempt) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin mark dlq: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := recordAttemptTx(ctx, tx, attempt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE events SET status = 'dlq', attempt_count = $2 WHERE id = $1`,
		eventID, attempt.AttemptNumber); err != nil {
		return fmt.Errorf("mark event %s dead-lettered: %w", eventID, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE endpoints SET consecutive_failures = consecutive_failures + 1 WHERE id = $1`,
		endpointID); err != nil {
		return fmt.Errorf("increment failure counter for endpoint %s: %w", endpointID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit mark dlq: %w", err)
	}
	return nil
}

// ReplayEvent returns a dead-lettered or permanently failed event to the ready
// set with a fresh attempt budget.
//
// The attempt history is NOT deleted. A replay is a new run of delivery, not a
// rewriting of what happened — the operator investigating why it failed the
// first time needs those rows, and the new attempts continue the numbering from
// where the old ones stopped so the audit trail stays ordered.
//
// Only terminal states can be replayed. Replaying an event that is mid-flight
// would race the worker holding it and could produce two live delivery chains
// for one event.
func (s *Store) ReplayEvent(ctx context.Context, eventID uuid.UUID) (*models.Event, error) {
	const q = `
		UPDATE events
		SET status = 'pending', attempt_count = 0, next_retry_at = now()
		WHERE id = $1 AND status IN ('dlq', 'delivered')
		RETURNING ` + eventColumns

	e, err := scanEvent(s.pool.QueryRow(ctx, q, eventID))
	if err == nil {
		return e, nil
	}
	// scanEvent has already normalized pgx.ErrNoRows to models.ErrNotFound, so
	// checking for the driver sentinel here would never match — an earlier
	// version did exactly that and could never return ErrNotReplayable at all,
	// which surfaced as a 500 where the API should answer 409.
	if !errors.Is(err, models.ErrNotFound) {
		return nil, fmt.Errorf("replay event %s: %w", eventID, err)
	}

	// The UPDATE matched nothing, which means either the event does not exist
	// or it is not in a replayable state. Those deserve different answers —
	// 404 sends an operator hunting for an id that is right there — so
	// establish which it is.
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM events WHERE id = $1`, eventID).Scan(&status); err != nil {
		if isNoRows(err) {
			return nil, models.ErrNotFound
		}
		return nil, fmt.Errorf("replay event %s: %w", eventID, err)
	}
	return nil, fmt.Errorf("%w: event is %s", models.ErrNotReplayable, status)
}

// NextAttemptNumber returns the attempt number a new delivery should use.
//
// Derived from the attempt history rather than from events.attempt_count, so a
// replay — which resets the counter but keeps the history — continues the
// numbering instead of colliding with the UNIQUE (event_id, attempt_number)
// constraint and silently dropping the row.
func (s *Store) NextAttemptNumber(ctx context.Context, eventID uuid.UUID) (int, error) {
	const q = `SELECT COALESCE(MAX(attempt_number), 0) + 1 FROM delivery_attempts WHERE event_id = $1`

	var n int
	if err := s.pool.QueryRow(ctx, q, eventID).Scan(&n); err != nil {
		return 0, fmt.Errorf("next attempt number for %s: %w", eventID, err)
	}
	return n, nil
}

// RescheduleWithoutAttempt defers an event without consuming an attempt.
//
// This is what a rate limit or an open circuit breaker produces. Neither is a
// failed delivery: the receiver never saw the request, so there is nothing to
// record in the attempt history and nothing that should count against the
// event's budget. Burning an attempt here would mean a slow consumer — a
// customer doing exactly what their rate limit says they may — eventually gets
// their events dead-lettered for being slow, which is absurd.
//
// The status goes back to 'failed' rather than 'pending' only because that is
// the state the relay's claim query looks for alongside 'pending'; both are
// treated identically. attempt_count is left untouched.
func (s *Store) RescheduleWithoutAttempt(ctx context.Context, eventID uuid.UUID, nextRetryAt time.Time) error {
	const q = `
		UPDATE events
		SET status = 'failed', next_retry_at = $2
		WHERE id = $1 AND status NOT IN ('delivered', 'dlq')`

	if _, err := s.pool.Exec(ctx, q, eventID, nextRetryAt); err != nil {
		return fmt.Errorf("reschedule event %s: %w", eventID, err)
	}
	return nil
}

// OldestBacklogAge returns how long the oldest undelivered event has been
// waiting since it was ingested.
//
// This is the SLO metric, and it is measured from created_at rather than from
// when the event entered the queue, because that is what a customer
// experiences: the clock starts when they handed us the event, not when we got
// around to scheduling it.
//
// It reads from Postgres rather than from the stream deliberately. The stream
// only knows about work it currently holds, so a Valkey restart would reset
// this gauge to zero and make a large backlog look healthy — the exact moment
// the number matters most. Postgres holds the durable truth.
//
// Returns 0 when nothing is outstanding.
func (s *Store) OldestBacklogAge(ctx context.Context) (time.Duration, error) {
	const q = `
		SELECT COALESCE(EXTRACT(EPOCH FROM (now() - MIN(created_at))), 0)
		FROM events
		WHERE status IN ('pending', 'failed', 'delivering')`

	var seconds float64
	if err := s.pool.QueryRow(ctx, q).Scan(&seconds); err != nil {
		return 0, fmt.Errorf("oldest backlog age: %w", err)
	}
	if seconds < 0 {
		seconds = 0
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// ActiveEndpointIDs lists endpoints for the per-endpoint breaker gauge.
//
// Bounded by limit because the gauge produces one series per endpoint. That is
// acceptable for the tens of endpoints this service is built for, and would be
// a cardinality problem at thousands — which is why the bound exists and is
// documented rather than left implicit.
func (s *Store) ActiveEndpointIDs(ctx context.Context, limit int) ([]uuid.UUID, error) {
	const q = `SELECT id FROM endpoints WHERE is_active ORDER BY created_at LIMIT $1`

	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list active endpoint ids: %w", err)
	}
	defer rows.Close()

	out := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan endpoint id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
