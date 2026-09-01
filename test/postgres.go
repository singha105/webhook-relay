// Package test provides integration-test helpers.
//
// Everything here talks to a real Postgres in a container via testcontainers-go.
// We do not mock the database: most of the behaviour worth testing in the store
// layer *is* database behaviour — CHECK constraints, ON CONFLICT arbitration,
// cascade deletes, and the visibility rules that make the idempotency race
// interesting. A mock would assert that our fake behaves like our fake.
package test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/singha105/webhook-relay/internal/store"
)

// containerOnce ensures one Postgres container is shared by every test in a
// package run. Starting a container per test would dominate the runtime; each
// test gets an isolated schema instead, which is both faster and a stronger
// isolation guarantee than truncating shared tables.
var (
	containerOnce sync.Once
	sharedDSN     string
	containerErr  error
)

// startPostgres boots postgres:16 once per test binary and returns its DSN.
func startPostgres(ctx context.Context) (string, error) {
	containerOnce.Do(func() {
		ctr, err := tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("webhook_relay"),
			tcpostgres.WithUsername("webhook_relay"),
			tcpostgres.WithPassword("webhook_relay"),
			// Two occurrences: the entrypoint starts the server once to run
			// init scripts and again for real. Waiting for the first would
			// hand back a DSN that stops working a moment later.
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(120*time.Second),
			),
		)
		if err != nil {
			containerErr = fmt.Errorf("start postgres container: %w", err)
			return
		}
		dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			containerErr = fmt.Errorf("postgres connection string: %w", err)
			return
		}
		sharedDSN = dsn
	})
	return sharedDSN, containerErr
}

// migrationsDir locates the migrations directory relative to the caller's
// package, so tests work regardless of which directory `go test` runs from.
func migrationsDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "migrations")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
		dir = filepath.Dir(dir)
	}
	return "", fmt.Errorf("could not locate migrations directory from working directory")
}

// applyMigrations executes every *.up.sql file in order against the pool.
//
// We execute the golang-migrate files directly rather than linking the
// golang-migrate library. The .sql files stay the single source of truth for
// both this path and `make migrate-up`, so the two cannot drift; and pgx sends
// an argument-free Exec over the simple protocol, which handles the
// multi-statement transactions these files contain.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := migrationsDir()
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no *.up.sql files found in %s", dir)
	}
	// Glob returns lexical order, which matches golang-migrate's zero-padded
	// numeric prefixes.
	for _, f := range files {
		sqlBytes, err := os.ReadFile(f) //nolint:gosec // paths come from our own repo
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

// NewStore returns a Store backed by a freshly migrated, isolated schema.
//
// Each test gets its own Postgres schema rather than its own database or a
// TRUNCATE between tests: creating a schema is cheap, and search_path
// isolation means two tests running in parallel cannot see each other's rows
// even though they share a container.
func NewStore(t *testing.T) *store.Store {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	ctx := context.Background()
	dsn, err := startPostgres(ctx)
	if err != nil {
		t.Fatalf("postgres container unavailable: %v\n\nIntegration tests need a running Docker daemon.", err)
	}

	schema := uniqueSchemaName(t)

	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	defer adminPool.Close()

	if _, createErr := adminPool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %q", schema)); createErr != nil {
		t.Fatalf("create schema %s: %v", schema, createErr)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	// Pin every connection in this pool to the test's own schema.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	// Enough headroom for the concurrency tests to actually run concurrently;
	// a pool of 1 would serialize them and quietly void the race test.
	cfg.MaxConns = 20

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if err := applyMigrations(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("apply migrations: %v", err)
	}

	// context.Background inside, not the test's context: cleanup runs after the
	// test finishes, when a test-scoped context is already cancelled and the
	// DROP SCHEMA would never reach the server.
	//nolint:contextcheck // a fresh context is required here, see above.
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		dropPool, dropErr := pgxpool.New(cleanupCtx, dsn)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(cleanupCtx, fmt.Sprintf("DROP SCHEMA %q CASCADE", schema))
	})

	return store.NewWithPool(pool)
}

// WarmPool forces the pool to establish n connections before a concurrency
// test starts.
//
// This is not an optimization, it is a correctness fix for the test itself.
// pgxpool opens connections lazily, so without warming, the first goroutine
// off the starting line gets the one established connection and completes its
// entire transaction while the others are still finishing TCP handshakes and
// authentication. The goroutines then never overlap, and a race test written
// against them will happily pass a badly broken implementation.
func WarmPool(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()

	ctx := context.Background()
	conns := make([]*pgxpool.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("warm pool: acquire %d/%d: %v", i+1, n, err)
		}
		// Force a real round trip; acquiring alone can hand back a connection
		// that has not completed its startup exchange.
		if _, err := c.Exec(ctx, "SELECT 1"); err != nil {
			t.Fatalf("warm pool: ping %d/%d: %v", i+1, n, err)
		}
		conns = append(conns, c)
	}
	// Hold them all at once, then release: releasing one at a time would let
	// the pool satisfy the next Acquire with the same connection.
	for _, c := range conns {
		c.Release()
	}
}
