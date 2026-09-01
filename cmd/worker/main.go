// Command worker will deliver webhooks. It does nothing today.
//
// Day 1 persists events with status 'pending' and stops there. This binary
// exists now so the layout, Dockerfile, and compose wiring are settled before
// the delivery loop lands on Day 2, not because it does any work.
//
// TODO(day-2): consume from queue.Queue, deliver over HTTP with an
// HMAC-SHA256 signature derived from the endpoint's signing secret, record a
// DeliveryAttempt per try, and move the event to delivered or failed.
// TODO(day-3): exponential backoff with jitter, a per-endpoint circuit breaker
// driven by endpoints.consecutive_failures, and a DLQ for exhausted retries.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/singha105/webhook-relay/internal/config"
	"github.com/singha105/webhook-relay/internal/httpapi"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}

	logger := httpapi.NewLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Deliberately loud. A worker that silently idles is indistinguishable
	// from one that is broken, and this one really does no work yet.
	logger.Warn("worker has no delivery loop yet; events will stay pending until day 2")

	<-ctx.Done()
	logger.Info("worker shutdown complete")
}
