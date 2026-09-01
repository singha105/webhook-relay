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

	"github.com/redis/go-redis/v9"

	"github.com/singha105/webhook-relay/internal/config"
	"github.com/singha105/webhook-relay/internal/delivery"
	"github.com/singha105/webhook-relay/internal/httpapi"
	"github.com/singha105/webhook-relay/internal/queue"
	"github.com/singha105/webhook-relay/internal/relay"
	"github.com/singha105/webhook-relay/internal/store"
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

	pool := worker.New(st, q, client, deduper,
		logger.With(slog.String("component", "worker")),
		hostname,
		worker.Config{
			Concurrency:  cfg.WorkerConcurrency,
			StaleTimeout: cfg.StaleClaimTimeout,
			Backoff:      delivery.NewBackoff(cfg.RetryBaseDelay, cfg.RetryMaxDelay, cfg.MaxAttempts),
		})

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

	<-ctx.Done()
	logger.Info("shutdown signal received; finishing in-flight deliveries")

	// No timeout on this wait. A delivery already in flight is bounded by the
	// delivery timeout, and cutting it short would leave an unacked entry to be
	// reclaimed and delivered a second time — turning a clean shutdown into a
	// duplicate. Kubernetes' terminationGracePeriodSeconds is the real bound.
	wg.Wait()
	logger.Info("worker shutdown complete")
	return nil
}
