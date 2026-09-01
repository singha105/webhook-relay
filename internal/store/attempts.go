package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/singha105/webhook-relay/internal/models"
)

const attemptColumns = `id, event_id, attempt_number, status_code, response_body,
	error_message, duration_ms, attempted_at`

// RecordAttempt appends one delivery attempt.
//
// The response body is truncated in Go before it reaches the driver; the
// CHECK constraint on the column is a backstop, not the primary defence, so a
// long body produces a short row rather than a failed insert.
//
// ON CONFLICT DO NOTHING on (event_id, attempt_number) makes this idempotent:
// a worker that crashes after writing attempt 3 and then retries must not
// create a second attempt 3. Here DO NOTHING is correct, unlike in CreateEvent
// — the caller does not need the surviving row back, only the guarantee that
// exactly one exists.
func (s *Store) RecordAttempt(ctx context.Context, a models.DeliveryAttempt) error {
	return recordAttemptOn(ctx, s.pool, a)
}

// recordAttemptTx inserts an attempt inside a caller's transaction, so the
// attempt row and the resulting event-status change commit together.
func recordAttemptTx(ctx context.Context, tx pgx.Tx, a models.DeliveryAttempt) error {
	return recordAttemptOn(ctx, tx, a)
}

// execer is the subset of pgxpool.Pool and pgx.Tx that recordAttemptOn needs,
// so one statement serves both the standalone and the transactional path and
// they cannot drift.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func recordAttemptOn(ctx context.Context, db execer, a models.DeliveryAttempt) error {
	const q = `
		INSERT INTO delivery_attempts
			(event_id, attempt_number, status_code, response_body, error_message, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (event_id, attempt_number) DO NOTHING`

	_, err := db.Exec(ctx, q,
		a.EventID, a.AttemptNumber, a.StatusCode,
		models.TruncateResponseBody(a.ResponseBody), a.ErrorMessage, a.DurationMS,
	)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("record attempt for event %s: %w", a.EventID, models.ErrNotFound)
		}
		return fmt.Errorf("record attempt for event %s: %w", a.EventID, err)
	}
	return nil
}

// ListAttempts returns every attempt for an event, oldest first. Served by the
// UNIQUE (event_id, attempt_number) index, which is why no separate index on
// event_id exists.
func (s *Store) ListAttempts(ctx context.Context, eventID uuid.UUID) ([]models.DeliveryAttempt, error) {
	const q = `
		SELECT ` + attemptColumns + `
		FROM delivery_attempts
		WHERE event_id = $1
		ORDER BY attempt_number`

	rows, err := s.pool.Query(ctx, q, eventID)
	if err != nil {
		return nil, fmt.Errorf("list attempts for event %s: %w", eventID, err)
	}
	defer rows.Close()

	out := make([]models.DeliveryAttempt, 0)
	for rows.Next() {
		var a models.DeliveryAttempt
		if err := rows.Scan(&a.ID, &a.EventID, &a.AttemptNumber, &a.StatusCode,
			&a.ResponseBody, &a.ErrorMessage, &a.DurationMS, &a.AttemptedAt); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list attempts for event %s: %w", eventID, err)
	}
	return out, nil
}
