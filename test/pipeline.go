package test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/singha105/webhook-relay/internal/breaker"
	"github.com/singha105/webhook-relay/internal/delivery"
	"github.com/singha105/webhook-relay/internal/queue"
	"github.com/singha105/webhook-relay/internal/ratelimit"
	"github.com/singha105/webhook-relay/internal/relay"
	"github.com/singha105/webhook-relay/internal/store"
	"github.com/singha105/webhook-relay/internal/worker"
	"github.com/singha105/webhook-relay/test/sink"
)

// Pipeline is a complete running system for integration tests: a real
// Postgres, a real Valkey, a real HTTP sink, the outbox relay, and a worker
// pool — all wired exactly as the worker binary wires them.
//
// Nothing here is faked. The point of these tests is the interaction between
// the parts, which is where the bugs actually live; a fake queue or a stubbed
// HTTP client would test the mock's behaviour instead.
type Pipeline struct {
	Store *store.Store
	Queue *queue.ValkeyQueue
	Sink  *sink.Sink
	// SinkURL is the address the delivery worker will POST to.
	SinkURL string
	// Backoff is the policy the pool was built with, so tests can assert
	// against the same ceilings the worker uses.
	Backoff delivery.Backoff
	// Pool is exposed so shutdown tests can drive Drain directly.
	Pool *worker.Pool
	// Breaker and Limiter are exposed so tests can inspect and reset them.
	Breaker *breaker.Breaker
	Limiter *ratelimit.Limiter

	cancel context.CancelFunc
	done   chan struct{}
}

// PipelineConfig tunes the test system. Zero values give fast, test-shaped
// defaults rather than production ones.
type PipelineConfig struct {
	// Concurrency is the worker pool size. Default 4.
	Concurrency int
	// MaxAttempts before the DLQ. Default 3, so exhaustion tests are quick.
	MaxAttempts int
	// RetryBase is the backoff base. Default 10ms, so a full retry chain
	// finishes in well under a second.
	RetryBase time.Duration
	// RetryMax caps the backoff window. Default 200ms.
	RetryMax time.Duration
	// DeliveryTimeout bounds one outbound call. Default 2s.
	DeliveryTimeout time.Duration
	// StaleTimeout is the queue reclaim threshold. Default 5s.
	StaleTimeout time.Duration
	// DedupEnabled controls the delivery dedup guard. Default true.
	DedupEnabled *bool
	// SinkSecret, when set, makes the sink verify signatures.
	VerifySignatures bool
	// Logs, when true, sends worker logs to the test log instead of discarding
	// them. Useful when a test is failing and you need to see why.
	Logs bool

	// RateLimitEnabled turns the token bucket on. Off by default so tests that
	// are not about rate limiting are not throttled by it.
	RateLimitEnabled bool
	// BreakerEnabled turns the circuit breaker on, likewise off by default.
	BreakerEnabled bool
	// BreakerThreshold and BreakerCooldown configure the breaker when enabled.
	BreakerThreshold int
	BreakerCooldown  time.Duration
	// DrainTimeout bounds the shutdown drain. Default 5s for tests.
	DrainTimeout time.Duration
}

func (c PipelineConfig) withDefaults() PipelineConfig {
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.RetryBase <= 0 {
		c.RetryBase = 10 * time.Millisecond
	}
	if c.RetryMax <= 0 {
		c.RetryMax = 200 * time.Millisecond
	}
	if c.DeliveryTimeout <= 0 {
		c.DeliveryTimeout = 2 * time.Second
	}
	if c.StaleTimeout <= 0 {
		c.StaleTimeout = 5 * time.Second
	}
	if c.DedupEnabled == nil {
		enabled := true
		c.DedupEnabled = &enabled
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = 5
	}
	if c.BreakerCooldown <= 0 {
		c.BreakerCooldown = 300 * time.Millisecond
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = 5 * time.Second
	}
	return c
}

// NewPipeline builds and starts a full system. It stops on test cleanup.
func NewPipeline(t *testing.T, cfg PipelineConfig) *Pipeline {
	t.Helper()
	cfg = cfg.withDefaults()

	st := NewStore(t)
	q := NewQueue(t)

	s := sink.New()
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if cfg.Logs {
		logger = slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	redisClient := redis.NewClient(mustRedisOptions(t))
	t.Cleanup(func() { _ = redisClient.Close() })

	backoff := delivery.NewBackoff(cfg.RetryBase, cfg.RetryMax, cfg.MaxAttempts)

	client := delivery.NewClient(delivery.ClientConfig{
		Timeout:             cfg.DeliveryTimeout,
		MaxIdleConnsPerHost: cfg.Concurrency,
		MaxRetryAfter:       cfg.RetryMax,
	})
	t.Cleanup(client.CloseIdleConnections)

	deduper := delivery.NewDeduper(redisClient, time.Minute, *cfg.DedupEnabled)

	r := relay.New(st, q, logger.With(slog.String("component", "relay")), relay.Config{
		// Fast poll so tests are not dominated by the relay's tick.
		PollInterval: 20 * time.Millisecond,
		BatchSize:    50,
		// Long enough that no healthy test delivery is ever requeued
		// underneath the worker performing it.
		Lease:         30 * time.Second,
		SweepInterval: time.Second,
	})

	limiter := ratelimit.New(redisClient, 1.0, cfg.RateLimitEnabled)
	brk := breaker.New(redisClient, breaker.Config{
		Threshold: cfg.BreakerThreshold,
		Cooldown:  cfg.BreakerCooldown,
		Enabled:   cfg.BreakerEnabled,
	})

	pool := worker.New(worker.Deps{
		Store:   st,
		Queue:   q,
		Client:  client,
		Deduper: deduper,
		Limiter: limiter,
		Breaker: brk,
		Logger:  logger.With(slog.String("component", "worker")),
		Name:    "test",
	}, worker.Config{
		Concurrency:     cfg.Concurrency,
		PollInterval:    20 * time.Millisecond,
		BatchSize:       2,
		StaleTimeout:    cfg.StaleTimeout,
		ReclaimInterval: 500 * time.Millisecond,
		Backoff:         backoff,
		DrainTimeout:    cfg.DrainTimeout,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)

	go func() { _ = r.Run(ctx); done <- struct{}{} }()
	go func() { _ = pool.Run(ctx); done <- struct{}{} }()

	p := &Pipeline{
		Store: st, Queue: q, Sink: s, SinkURL: srv.URL + "/hook",
		Backoff: backoff, Pool: pool, Breaker: brk, Limiter: limiter,
		cancel: cancel, done: done,
	}
	t.Cleanup(p.Stop)
	return p
}

// Stop shuts the pipeline down and waits for both loops to exit.
func (p *Pipeline) Stop() {
	if p.cancel == nil {
		return
	}
	p.cancel()
	p.cancel = nil
	for i := 0; i < 2; i++ {
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			return
		}
	}
}

// EnableSignatureVerification makes the sink verify every delivery against
// secret and record the result.
func (p *Pipeline) EnableSignatureVerification(secret string) {
	p.Sink.SetSecret(secret)
}

// testWriter routes slog output into the test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Logf("%s", b)
	return len(b), nil
}

func mustRedisOptions(t *testing.T) *redis.Options {
	t.Helper()
	url, err := startValkey(context.Background())
	if err != nil {
		t.Fatalf("valkey unavailable: %v", err)
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse valkey url: %v", err)
	}
	return opt
}
