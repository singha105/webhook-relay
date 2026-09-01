// Command sink runs the controllable webhook receiver as a standalone server.
//
// It exists for the manual Day 2 verification, where an endpoint has to be
// flipped from failing to healthy while the worker keeps running. It is a test
// fixture, not part of the service, which is why it lives under /test.
//
//	go run ./test/sink/cmd/sink -addr :9090
//	curl -X POST localhost:9090/_control/behavior -d '{"status":500}'
//	curl -s localhost:9090/_control/stats | jq
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":9090", "listen address")
	secret := flag.String("secret", "", "signing secret; when set, every delivery's signature is verified and the result recorded")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	s := newSink(*secret)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("sink listening", slog.String("addr", *addr), slog.Bool("verifying_signatures", *secret != ""))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("sink failed", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	logger.Info("sink stopped")
}
