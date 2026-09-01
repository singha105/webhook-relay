// Package store is the Postgres access layer. It owns every SQL statement in
// the service; no other package builds queries.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgErrUniqueViolation is the SQLSTATE Postgres raises when a UNIQUE index
// rejects a row. Ingest depends on catching exactly this code.
const pgErrUniqueViolation = "23505"

// pgErrForeignKeyViolation is raised when an event references an endpoint that
// does not exist.
const pgErrForeignKeyViolation = "23503"

// Store holds the connection pool and exposes the repository methods.
type Store struct {
	pool *pgxpool.Pool
}

// New opens a pool and verifies connectivity before returning. Failing fast at
// boot is what makes the Kubernetes readiness probe meaningful: a pod that
// cannot reach its database should never report ready.
func New(ctx context.Context, databaseURL string, maxConns int32, connectTimeout time.Duration) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = maxConns
	// Recycle connections so a long-lived pod does not pin backends that
	// Postgres would rather reclaim, and so a rolling database restart is
	// picked up without bouncing the app.
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// NewWithPool wraps an existing pool. Integration tests use this to hand in a
// pool pointed at a testcontainer.
func NewWithPool(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the underlying pool for test helpers and migration running.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Ping is the readiness check. It is deliberately a real round trip rather
// than a pool-state inspection: a pool can hold connections to a database that
// has since stopped accepting queries.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// isUniqueViolation reports whether err is a UNIQUE constraint violation, and
// if so which constraint. Ingest uses this to turn a lost insert race into a
// 200 rather than a 500.
func isUniqueViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrUniqueViolation {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// isForeignKeyViolation reports whether err is an FK violation, which for our
// schema always means "the referenced endpoint does not exist".
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgErrForeignKeyViolation
}

// isNoRows normalizes pgx's sentinel so callers compare against models.ErrNotFound.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
