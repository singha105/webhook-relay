package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/singha105/webhook-relay/internal/models"
	"github.com/singha105/webhook-relay/internal/telemetry"
)

// ErrEndpointNotFound is returned by CreateEvent when the referenced endpoint
// does not exist. It is distinct from models.ErrNotFound so the handler can
// answer 422 (the body names a bad endpoint) rather than 404 (this URL has no
// resource).
var ErrEndpointNotFound = fmt.Errorf("endpoint does not exist")

const eventColumns = `id, endpoint_id, event_type, payload, status, idempotency_key,
	created_at, attempt_count, next_retry_at`

func scanEvent(row pgx.Row) (*models.Event, error) {
	var e models.Event
	var payload []byte
	err := row.Scan(&e.ID, &e.EndpointID, &e.EventType, &payload, &e.Status,
		&e.IdempotencyKey, &e.CreatedAt, &e.AttemptCount, &e.NextRetryAt)
	if err != nil {
		if isNoRows(err) {
			return nil, models.ErrNotFound
		}
		return nil, err
	}
	e.Payload = json.RawMessage(payload)
	return &e, nil
}

// CreateEvent persists an event and reports whether it was newly created.
//
// The idempotency race is resolved by the database, not by the application.
// A SELECT-then-INSERT has a window between the two statements in which a
// concurrent request can insert the same key, and closing that window in
// application code would need a distributed lock we do not want on the ingest
// path. Instead we INSERT unconditionally against the partial unique index
// (endpoint_id, idempotency_key) and let Postgres arbitrate.
//
// ON CONFLICT DO UPDATE rather than DO NOTHING, even though the update is a
// no-op that writes the key back to itself. DO NOTHING returns zero rows on
// conflict, forcing a follow-up SELECT that may run before the winning
// transaction has committed and therefore may find nothing — the race would
// reappear one statement later. DO UPDATE takes a row lock, waits for the
// winner to commit, and hands back the surviving row. We pay one dead tuple
// per duplicate for a guarantee that holds under concurrency.
//
// "Was it created?" is answered by comparing the returned ID to the UUIDv7 we
// generated: the database can only return our ID if our INSERT was the one
// that landed. That avoids the xmax=0 trick, which relies on an internal
// system column.
func (s *Store) CreateEvent(ctx context.Context, id, endpointID uuid.UUID, eventType string, payload json.RawMessage, idempotencyKey *string) (event *models.Event, created bool, err error) {
	const q = `
		INSERT INTO events (id, endpoint_id, event_type, payload, idempotency_key, trace_context)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (endpoint_id, idempotency_key) WHERE idempotency_key IS NOT NULL
		DO UPDATE SET idempotency_key = events.idempotency_key
		RETURNING ` + eventColumns

	// The caller's trace context is persisted with the event. The relay that
	// eventually enqueues this row runs later, in another goroutine and usually
	// another process, so there is no in-memory context for it to inherit —
	// this column is the only thing that can carry the trace across the outbox.
	var traceJSON []byte
	if carrier := telemetry.InjectContext(ctx); len(carrier) > 0 {
		if encoded, mErr := json.Marshal(carrier); mErr == nil {
			traceJSON = encoded
		}
	}

	e, err := scanEvent(s.pool.QueryRow(ctx, q, id, endpointID, eventType, []byte(payload), idempotencyKey, traceJSON))
	if err != nil {
		if isForeignKeyViolation(err) {
			return nil, false, ErrEndpointNotFound
		}
		return nil, false, fmt.Errorf("create event: %w", err)
	}
	return e, e.ID == id, nil
}

// GetEvent returns one event without its attempts.
func (s *Store) GetEvent(ctx context.Context, id uuid.UUID) (*models.Event, error) {
	const q = `SELECT ` + eventColumns + ` FROM events WHERE id = $1`

	e, err := scanEvent(s.pool.QueryRow(ctx, q, id))
	if err != nil {
		return nil, fmt.Errorf("get event %s: %w", id, err)
	}
	return e, nil
}

// GetEventWithAttempts returns an event and its full delivery history.
//
// Two queries rather than one LEFT JOIN. A join would repeat every event
// column once per attempt and then need de-duplication in Go; with attempts
// capped at a handful per event, two indexed lookups are both cheaper and
// considerably easier to read.
func (s *Store) GetEventWithAttempts(ctx context.Context, id uuid.UUID) (*models.EventWithAttempts, error) {
	event, err := s.GetEvent(ctx, id)
	if err != nil {
		return nil, err
	}
	attempts, err := s.ListAttempts(ctx, id)
	if err != nil {
		return nil, err
	}
	return &models.EventWithAttempts{Event: *event, Attempts: attempts}, nil
}

// ListDueEventsForEndpoint returns the events for one endpoint that are ready
// for delivery, soonest first.
//
// Served by events_due_by_endpoint_idx: endpoint_id leads because it is the
// equality predicate, next_retry_at follows so it supplies the ORDER BY and
// LIMIT can short-circuit without a sort.
//
// 'failed' is included alongside 'pending' because an event awaiting its third
// retry sits in 'failed' — filtering on 'pending' alone, as the day-1 version
// did, would have silently hidden every event that had ever been attempted.
// This is a read-only view for operators and tests; the relay claims work
// through ClaimDueEvents, which locks.
func (s *Store) ListDueEventsForEndpoint(ctx context.Context, endpointID uuid.UUID, limit int) ([]models.Event, error) {
	const q = `
		SELECT ` + eventColumns + `
		FROM events
		WHERE endpoint_id = $1
		  AND status IN ('pending', 'failed')
		  AND next_retry_at <= now()
		ORDER BY next_retry_at
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, endpointID, limit)
	if err != nil {
		return nil, fmt.Errorf("list due events: %w", err)
	}
	defer rows.Close()

	out := make([]models.Event, 0)
	for rows.Next() {
		var e models.Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.EndpointID, &e.EventType, &payload, &e.Status,
			&e.IdempotencyKey, &e.CreatedAt, &e.AttemptCount, &e.NextRetryAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due events: %w", err)
	}
	return out, nil
}

// UpdateEventStatus moves an event to a new status. Day 2 uses it; Day 1 keeps
// it exercised by tests so the CHECK constraint stays honest.
func (s *Store) UpdateEventStatus(ctx context.Context, id uuid.UUID, status models.EventStatus) error {
	if !status.Valid() {
		return fmt.Errorf("update event status: %q is not a valid status", status)
	}
	const q = `UPDATE events SET status = $2 WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q, id, string(status))
	if err != nil {
		return fmt.Errorf("update event %s status: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update event %s status: %w", id, models.ErrNotFound)
	}
	return nil
}
