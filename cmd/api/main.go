// Command api runs the webhook-relay ingest and management HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/singha105/webhook-relay/internal/breaker"
	"github.com/singha105/webhook-relay/internal/config"
	"github.com/singha105/webhook-relay/internal/httpapi"
	"github.com/singha105/webhook-relay/internal/store"
	"github.com/singha105/webhook-relay/internal/telemetry"
)

func main() {
	// Handled before anything else: the distroless runtime image has no shell
	// and no curl, so the container healthcheck invokes the binary itself.
	if handled, code := httpapi.RunSelfCheckFlag("http://127.0.0.1:8080/readyz"); handled {
		os.Exit(code)
	}
	if err := run(); err != nil {
		// The logger may not exist yet if config failed, so this one goes to
		// stderr directly.
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
	logger.Info("starting webhook-relay api", slog.Any("config", cfg.Redacted()))

	// Signal context first, so a Ctrl-C during a slow database connect still
	// exits promptly instead of waiting out the connect timeout.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBConnectTimeout)
	if err != nil {
		return err
	}
	defer st.Close()
	logger.Info("connected to postgres", slog.Int("max_conns", int(cfg.DBMaxConns)))

	tracing, shutdownTracing, traceErr := telemetry.SetupTracing(ctx, telemetry.TracingConfig{
		Enabled:        cfg.TracingEnabled,
		Endpoint:       cfg.OTLPEndpoint,
		ServiceName:    "webhook-relay-api",
		ServiceVersion: cfg.ServiceVersion,
		SampleRatio:    cfg.TraceSampleRatio,
	}, logger)
	if traceErr != nil {
		return fmt.Errorf("setup tracing: %w", traceErr)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if flushErr := shutdownTracing(flushCtx); flushErr != nil {
			logger.Warn("could not flush traces on shutdown", slog.Any("error", flushErr))
		}
	}()

	metrics, metricsErr := telemetry.SetupMetrics(telemetry.MetricsConfig{
		ServiceName:    "webhook-relay-api",
		ServiceVersion: cfg.ServiceVersion,
	}, logger)
	if metricsErr != nil {
		return fmt.Errorf("setup metrics: %w", metricsErr)
	}
	defer func() { _ = metrics.Shutdown(context.Background()) }()

	// The API has no queue handle, so it reports the backlog gauge from
	// Postgres — which is the durable source anyway, and the one that survives
	// Valkey being restarted.
	metrics.SetOldestAgeSource(func(ctx context.Context) (float64, error) {
		age, err := st.OldestBacklogAge(ctx)
		return age.Seconds(), err
	})

	redisOpt, redisErr := redis.ParseURL(cfg.ValkeyURL)
	if redisErr != nil {
		return fmt.Errorf("parse valkey url: %w", redisErr)
	}
	redisClient := redis.NewClient(redisOpt)
	defer func() { _ = redisClient.Close() }()

	brk := breaker.New(redisClient, breaker.Config{
		Threshold: cfg.BreakerThreshold,
		Cooldown:  cfg.BreakerCooldown,
		Enabled:   cfg.BreakerEnabled,
	})

	// Readiness for the API. Unlike the worker, it DOES include the backlog
	// check: shedding ingest while the workers catch up is better than
	// accepting events we already know we cannot deliver on time.
	ready := httpapi.NewReadinessChecker().
		Add("postgres", st.Ping).
		Add("valkey", func(ctx context.Context) error { return redisClient.Ping(ctx).Err() }).
		AddBacklogCheck(cfg.ReadinessMaxBacklogAge, st.OldestBacklogAge)

	apiServer := httpapi.NewServer(st, logger, httpapi.ServerConfig{RequestTimeout: cfg.RequestTimeout}).
		WithBreaker(brk).
		WithMetrics(metrics).
		WithTracer(tracing.Tracer).
		WithReadiness(ready)

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: apiServer.Routes(),

		// Without these a slow or malicious client can hold a connection open
		// indefinitely and exhaust the listener.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", slog.String("addr", cfg.HTTPAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections",
			slog.String("timeout", cfg.ShutdownTimeout.String()))
	}

	// A fresh context: the signal context is already cancelled, and passing it
	// to Shutdown would abort the drain immediately.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed, forcing close", slog.Any("error", err))
		return srv.Close()
	}
	logger.Info("shutdown complete")
	return nil
}
