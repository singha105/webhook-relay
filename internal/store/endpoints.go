package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/singha105/webhook-relay/internal/models"
)

// endpointColumns is the canonical projection. Kept in one place so a schema
// change cannot leave one query selecting a stale column list.
const endpointColumns = `id, url, description, signing_secret, is_active,
	rate_limit_per_sec, consecutive_failures, created_at, updated_at`

func scanEndpoint(row pgx.Row) (*models.Endpoint, error) {
	var e models.Endpoint
	err := row.Scan(
		&e.ID, &e.URL, &e.Description, &e.SigningSecret, &e.IsActive,
		&e.RateLimitPerSec, &e.ConsecutiveFailures, &e.CreatedAt, &e.UpdatedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return nil, models.ErrNotFound
		}
		return nil, err
	}
	return &e, nil
}

// CreateEndpoint inserts an endpoint and returns it with database-assigned
// values (id, timestamps) populated.
func (s *Store) CreateEndpoint(ctx context.Context, url, description, signingSecret string, rateLimitPerSec int) (*models.Endpoint, error) {
	const q = `
		INSERT INTO endpoints (url, description, signing_secret, rate_limit_per_sec)
		VALUES ($1, $2, $3, $4)
		RETURNING ` + endpointColumns

	e, err := scanEndpoint(s.pool.QueryRow(ctx, q, url, description, signingSecret, rateLimitPerSec))
	if err != nil {
		return nil, fmt.Errorf("create endpoint: %w", err)
	}
	return e, nil
}

// GetEndpoint returns one endpoint, or models.ErrNotFound.
func (s *Store) GetEndpoint(ctx context.Context, id uuid.UUID) (*models.Endpoint, error) {
	const q = `SELECT ` + endpointColumns + ` FROM endpoints WHERE id = $1`

	e, err := scanEndpoint(s.pool.QueryRow(ctx, q, id))
	if err != nil {
		return nil, fmt.Errorf("get endpoint %s: %w", id, err)
	}
	return e, nil
}

// ListEndpoints returns endpoints newest-first, capped at limit.
//
// Offset pagination is a known limitation: it drifts when rows are inserted
// mid-scan and degrades at high offsets. It is honest for Day 1 because the
// endpoint count is small by nature — endpoints are registered by humans, not
// by traffic. The events table, which does grow with traffic, would need
// keyset pagination instead.
func (s *Store) ListEndpoints(ctx context.Context, limit, offset int) ([]models.Endpoint, error) {
	const q = `
		SELECT ` + endpointColumns + `
		FROM endpoints
		ORDER BY created_at DESC, id DESC
		LIMIT $1 OFFSET $2`

	rows, err := s.pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	defer rows.Close()

	// Non-nil empty slice so the JSON encoder emits [] rather than null.
	out := make([]models.Endpoint, 0)
	for rows.Next() {
		var e models.Endpoint
		if err := rows.Scan(
			&e.ID, &e.URL, &e.Description, &e.SigningSecret, &e.IsActive,
			&e.RateLimitPerSec, &e.ConsecutiveFailures, &e.CreatedAt, &e.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan endpoint: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	return out, nil
}

// UpdateEndpoint applies a partial update.
//
// COALESCE($n, column) rather than a dynamically built SET clause: the
// statement text is constant, so Postgres can reuse a prepared plan and there
// is no string concatenation anywhere near user input. A nil parameter leaves
// the column alone, which is what makes this a real PATCH. updated_at is left
// to the database trigger.
func (s *Store) UpdateEndpoint(ctx context.Context, id uuid.UUID, req models.UpdateEndpointRequest) (*models.Endpoint, error) {
	const q = `
		UPDATE endpoints
		SET url                = COALESCE($2, url),
		    description        = COALESCE($3, description),
		    is_active          = COALESCE($4, is_active),
		    rate_limit_per_sec = COALESCE($5, rate_limit_per_sec)
		WHERE id = $1
		RETURNING ` + endpointColumns

	e, err := scanEndpoint(s.pool.QueryRow(ctx, q, id,
		req.URL, req.Description, req.IsActive, req.RateLimitPerSec))
	if err != nil {
		return nil, fmt.Errorf("update endpoint %s: %w", id, err)
	}
	return e, nil
}

// DeleteEndpoint removes an endpoint. Its events and their attempts go with it
// via ON DELETE CASCADE. Returns models.ErrNotFound if nothing was deleted, so
// the caller can answer 404 rather than a misleading 204.
func (s *Store) DeleteEndpoint(ctx context.Context, id uuid.UUID) error {
	const q = `DELETE FROM endpoints WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("delete endpoint %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete endpoint %s: %w", id, models.ErrNotFound)
	}
	return nil
}
