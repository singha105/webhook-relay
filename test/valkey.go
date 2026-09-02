package test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/singha105/webhook-relay/internal/queue"
)

var (
	valkeyOnce sync.Once
	valkeyURL  string
	valkeyErr  error
)

// startValkey boots one valkey:8 container per test binary.
//
// The real image, not a Redis image and not miniredis. Consumer-group
// semantics — XAUTOCLAIM idle accounting, XPENDING retry counts, group lag —
// are exactly what this project depends on and exactly what an in-memory fake
// gets subtly wrong.
func startValkey(ctx context.Context) (string, error) {
	valkeyOnce.Do(func() {
		ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "valkey/valkey:8-alpine",
				ExposedPorts: []string{"6379/tcp"},
				WaitingFor: wait.ForLog("Ready to accept connections").
					WithStartupTimeout(90 * time.Second),
			},
			Started: true,
		})
		if err != nil {
			valkeyErr = fmt.Errorf("start valkey container: %w", err)
			return
		}
		host, err := ctr.Host(ctx)
		if err != nil {
			valkeyErr = fmt.Errorf("valkey host: %w", err)
			return
		}
		port, err := ctr.MappedPort(ctx, "6379/tcp")
		if err != nil {
			valkeyErr = fmt.Errorf("valkey port: %w", err)
			return
		}
		valkeyURL = fmt.Sprintf("redis://%s:%s", host, port.Port())
	})
	return valkeyURL, valkeyErr
}

// NewQueue returns a ValkeyQueue on a stream key unique to this test.
//
// Isolation is by stream name rather than by Valkey's numbered logical
// databases: there are only 16 of those, so a suite with more parallel queue
// tests than that would silently start sharing state. A per-test key has no
// such ceiling. FLUSHDB between tests was the other option and is worse — it
// races with anything running in parallel.
func NewQueue(t *testing.T) *queue.ValkeyQueue {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	ctx := context.Background()
	baseURL, err := startValkey(ctx)
	if err != nil {
		t.Fatalf("valkey container unavailable: %v\n\nIntegration tests need a running Docker daemon.", err)
	}

	name := uniqueStreamName(t)
	q, err := queue.NewValkeyQueue(ctx, queue.ValkeyConfig{
		URL:    baseURL,
		MaxLen: 10000,
		Stream: name,
		Group:  name + ":workers",
	})
	if err != nil {
		t.Fatalf("create queue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

var streamCounter atomic.Uint64

// uniqueStreamName derives a collision-free stream key from the test name.
func uniqueStreamName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test:%s:%d", sanitizeIdent(t.Name()), streamCounter.Add(1))
}

// NewRedisClient returns a raw client on the shared Valkey container, for the
// components that use it directly rather than through the queue.
func NewRedisClient(t *testing.T) *redis.Client {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	url, err := startValkey(context.Background())
	if err != nil {
		t.Fatalf("valkey container unavailable: %v", err)
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse valkey url: %v", err)
	}
	client := redis.NewClient(opt)
	t.Cleanup(func() { _ = client.Close() })
	return client
}
