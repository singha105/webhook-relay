package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/singha105/webhook-relay/internal/models"
)

// ErrEndpointNotFound is returned by CreateEvent when the referenced endpoint
// does not exist. It is distinct from models.ErrNotFound so the handler can
// answer 422 (the body names a bad endpoint) rather than 404 (this URL has no
// resource).
var ErrEndpointNotFound = fmt.Errorf("endpoint does not exist")

const eventColumns = `id, endpoint_id, event_type, payload, status, idempotency_key, created_at`

func scanEvent(row pgx.Row) (*models.Event, error) {
	var e models.Event
	var payload []byte
	err := row.Scan(&e.ID, &e.EndpointID, &e.EventType, &payload, &e.Status, &e.IdempotencyKey, &e.CreatedAt)
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
		INSERT INTO events (id, endpoint_id, event_type, payload, idempotency_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (endpoint_id, idempotency_key) WHERE idempotency_key IS NOT NULL
		DO UPDATE SET idempotency_key = events.idempotency_key
		RETURNING ` + eventColumns

	e, err := scanEvent(s.pool.QueryRow(ctx, q, id, endpointID, eventType, []byte(payload), idempotencyKey))
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

// ListPendingEventsForEndpoint returns the oldest pending events for one
// endpoint. This is the query the schema's partial index
// events (endpoint_id, created_at) WHERE status='pending' exists to serve:
// the index supplies both the filter and the ordering, so the plan is an index
// scan with no sort node and LIMIT short-circuits it.
//
// Day 2's worker will need FOR UPDATE SKIP LOCKED here (or to consume from
// Valkey instead). Today nothing claims events, so this is a plain read used
// by tests and by the seed script.
func (s *Store) ListPendingEventsForEndpoint(ctx context.Context, endpointID uuid.UUID, limit int) ([]models.Event, error) {
	const q = `
		SELECT ` + eventColumns + `
		FROM events
		WHERE endpoint_id = $1 AND status = 'pending'
		ORDER BY created_at
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, endpointID, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending events: %w", err)
	}
	defer rows.Close()

	out := make([]models.Event, 0)
	for rows.Next() {
		var e models.Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.EndpointID, &e.EventType, &payload, &e.Status, &e.IdempotencyKey, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending events: %w", err)
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
