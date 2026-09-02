// Command worker delivers webhooks.
//
// It runs two things in one process:
//
//   - the outbox relay, which moves due events from Postgres into the queue;
//   - a pool of delivery goroutines, which consume the queue and make the
//     outbound HTTP calls.
//
// They are colocated because they scale together and because a deployment with
// one of them missing is silently broken — events would be enqueued and never
// delivered, or delivered by workers with nothing feeding them. Both are safe
// to run in several replicas: the relay claims with FOR UPDATE SKIP LOCKED and
// the workers share one consumer group.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/singha105/webhook-relay/internal/breaker"
	"github.com/singha105/webhook-relay/internal/config"
	"github.com/singha105/webhook-relay/internal/delivery"
	"github.com/singha105/webhook-relay/internal/httpapi"
	"github.com/singha105/webhook-relay/internal/queue"
	"github.com/singha105/webhook-relay/internal/ratelimit"
	"github.com/singha105/webhook-relay/internal/relay"
	"github.com/singha105/webhook-relay/internal/store"
	"github.com/singha105/webhook-relay/internal/telemetry"
	"github.com/singha105/webhook-relay/internal/worker"
)

func main() {
	if handled, code := runHealthcheck(); handled {
		os.Exit(code)
	}
	if err := run(); err != nil {
		slog.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := httpapi.NewLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	logger.Info("starting webhook-relay worker", slog.Any("config", cfg.Redacted()))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tracing, shutdownTracing, traceErr := telemetry.SetupTracing(ctx, telemetry.TracingConfig{
		Enabled:        cfg.TracingEnabled,
		Endpoint:       cfg.OTLPEndpoint,
		ServiceName:    "webhook-relay-worker",
		ServiceVersion: cfg.ServiceVersion,
		SampleRatio:    cfg.TraceSampleRatio,
	}, logger)
	if traceErr != nil {
		// Not fatal in spirit, but a misconfigured endpoint should be loud at
		// boot rather than silently dropping every trace.
		return fmt.Errorf("setup tracing: %w", traceErr)
	}
	defer func() {
		// A fresh context: the signal context is already cancelled by now, and
		// passing it would discard the spans we are trying to flush.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if flushErr := shutdownTracing(flushCtx); flushErr != nil {
			logger.Warn("could not flush traces on shutdown", slog.Any("error", flushErr))
		}
	}()

	metrics, metricsErr := telemetry.SetupMetrics(telemetry.MetricsConfig{
		ServiceName:    "webhook-relay-worker",
		ServiceVersion: cfg.ServiceVersion,
	}, logger)
	if metricsErr != nil {
		return fmt.Errorf("setup metrics: %w", metricsErr)
	}
	defer func() { _ = metrics.Shutdown(context.Background()) }()

	st, err := store.New(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBConnectTimeout)
	if err != nil {
		return err
	}
	defer st.Close()

	q, err := queue.NewValkeyQueue(ctx, queue.ValkeyConfig{
		URL: cfg.ValkeyURL,
		// Bounded so acknowledged history cannot grow without limit.
		MaxLen: 100000,
		// One connection per delivery goroutine, plus headroom for the relay
		// and the reclaim sweeper.
		PoolSize: cfg.WorkerConcurrency + 4,
	})
	if err != nil {
		return err
	}
	defer func() { _ = q.Close() }()

	// A separate client for the dedup keys. It shares the Valkey instance but
	// not the queue's pool, so a saturated queue cannot starve the guard.
	redisOpt, err := redis.ParseURL(cfg.ValkeyURL)
	if err != nil {
		return fmt.Errorf("parse valkey url for dedup: %w", err)
	}
	redisClient := redis.NewClient(redisOpt)
	defer func() { _ = redisClient.Close() }()

	deduper := delivery.NewDeduper(redisClient, cfg.DeliveryDedupTTL, cfg.DeliveryDedupEnabled)
	limiter := ratelimit.New(redisClient, cfg.RateLimitBurstFactor, cfg.RateLimitEnabled)
	brk := breaker.New(redisClient, breaker.Config{
		Threshold: cfg.BreakerThreshold,
		Cooldown:  cfg.BreakerCooldown,
		Enabled:   cfg.BreakerEnabled,
	})

	// Gauges are read at scrape time rather than sampled on a timer, so the
	// dashboard shows the queue as it is now, not as it was a minute ago.
	metrics.SetQueueDepthSource(func(ctx context.Context) (int64, error) {
		return q.Depth(ctx)
	})
	metrics.SetOldestAgeSource(func(ctx context.Context) (float64, error) {
		age, err := st.OldestBacklogAge(ctx)
		return age.Seconds(), err
	})
	metrics.SetBreakerStateSource(func(ctx context.Context) (map[string]float64, error) {
		ids, err := st.ActiveEndpointIDs(ctx, maxBreakerSeries)
		if err != nil {
			return nil, err
		}
		out := make(map[string]float64, len(ids))
		for _, id := range ids {
			state, err := brk.Current(ctx, id)
			if err != nil {
				continue
			}
			out[id.String()] = state.Numeric()
		}
		return out, nil
	})

	client := delivery.NewClient(delivery.ClientConfig{
		Timeout: cfg.DeliveryTimeout,
		// Enough idle connections for every goroutine to keep one open to a
		// busy endpoint, so a burst does not force fresh TLS handshakes.
		MaxIdleConnsPerHost: cfg.WorkerConcurrency,
		MaxIdleConns:        cfg.WorkerConcurrency * 8,
		MaxRetryAfter:       cfg.RetryMaxDelay,
	})
	defer client.CloseIdleConnections()

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "worker"
	}

	r := relay.New(st, q, logger.With(slog.String("component", "relay")), relay.Config{
		PollInterval: cfg.RelayPollInterval,
		BatchSize:    cfg.RelayBatchSize,
		Lease:        cfg.DeliveryLease,
	})

	pool := worker.New(worker.Deps{
		Store:   st,
		Queue:   q,
		Client:  client,
		Deduper: deduper,
		Limiter: limiter,
		Breaker: brk,
		Metrics: metrics,
		Tracer:  tracing.Tracer,
		Logger:  logger.With(slog.String("component", "worker")),
		Name:    hostname,
	}, worker.Config{
		Concurrency:  cfg.WorkerConcurrency,
		StaleTimeout: cfg.StaleClaimTimeout,
		Backoff:      delivery.NewBackoff(cfg.RetryBaseDelay, cfg.RetryMaxDelay, cfg.MaxAttempts),
		DrainTimeout: cfg.DrainTimeout,
	})

	// Readiness for the worker: it needs both dependencies to do any work at
	// all. The backlog check is deliberately NOT applied here — a worker with a
	// deep backlog is the one thing working on it, and pulling it out of the
	// Service would only slow recovery.
	ready := httpapi.NewReadinessChecker().
		Add("postgres", st.Ping).
		Add("valkey", q.Ping)

	ops := httpapi.NewOpsServer(httpapi.OpsConfig{
		Addr:    cfg.MetricsAddr,
		Metrics: metrics,
		Ready:   ready.Check,
		Logger:  logger,
	})
	opsErr := make(chan error, 1)
	ops.Start(opsErr)
	logger.Info("operational listener started",
		slog.String("addr", cfg.MetricsAddr),
		slog.String("endpoints", "/healthz /readyz /metrics"))
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ops.Shutdown(shutdownCtx)
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := r.Run(ctx); err != nil {
			logger.Error("relay exited with an error", slog.Any("error", err))
		}
	}()
	go func() {
		defer wg.Done()
		if err := pool.Run(ctx); err != nil {
			logger.Error("worker pool exited with an error", slog.Any("error", err))
		}
	}()

	select {
	case err := <-opsErr:
		return fmt.Errorf("operational listener failed: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutdown signal received; draining in-flight deliveries",
		slog.Duration("budget", cfg.DrainTimeout))

	// Drain BEFORE waiting on the goroutines. Drain stops them claiming new
	// work and waits for the deliveries they already hold, so by the time the
	// context-driven loops exit there is nothing half-finished. Doing it the
	// other way round would race: the loops would exit on ctx.Done() while a
	// delivery was still in flight, leaving it unacked and guaranteeing a
	// duplicate on the next reclaim.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.DrainTimeout+5*time.Second)
	defer cancelDrain()
	if err := pool.Drain(drainCtx); err != nil {
		logger.Warn("drain did not complete cleanly", slog.Any("error", err))
	}

	wg.Wait()
	logger.Info("worker shutdown complete")
	return nil
}

// maxBreakerSeries bounds the per-endpoint breaker gauge. One series per
// endpoint is fine for the tens this service targets and would be a
// cardinality problem at thousands, so the bound is explicit rather than
// implied.
const maxBreakerSeries = 500
