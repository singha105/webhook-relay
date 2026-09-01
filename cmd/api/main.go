// Command api runs the webhook-relay ingest and management HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/singha105/webhook-relay/internal/config"
	"github.com/singha105/webhook-relay/internal/httpapi"
	"github.com/singha105/webhook-relay/internal/store"
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

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: httpapi.NewServer(st, logger, httpapi.ServerConfig{RequestTimeout: cfg.RequestTimeout}).Routes(),

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
