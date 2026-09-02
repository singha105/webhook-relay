package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/singha105/webhook-relay/internal/telemetry"
)

// OpsServer is the operational HTTP surface for a process that has no API of
// its own — currently the worker.
//
// The worker needs a listener for three reasons that all appear on Day 3:
// Prometheus has to scrape /metrics, Kubernetes has to probe liveness and
// readiness separately, and an operator needs somewhere to look. Bundling them
// on one small mux keeps that surface explicit rather than scattered.
type OpsServer struct {
	srv *http.Server
}

// OpsConfig configures the operational listener.
type OpsConfig struct {
	Addr    string
	Metrics *telemetry.Metrics
	// Ready reports whether this process should receive traffic. Nil means
	// always ready.
	Ready  func(context.Context) error
	Logger *slog.Logger
}

// NewOpsServer builds the listener.
func NewOpsServer(cfg OpsConfig) *OpsServer {
	mux := http.NewServeMux()

	// Liveness. Touches nothing. See the comment on the API's healthz for why
	// this must not check dependencies.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if cfg.Ready == nil {
			writeJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := cfg.Ready(ctx); err != nil {
			writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{
				"status": "unavailable",
				"reason": err.Error(),
			})
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
	})

	if cfg.Metrics != nil {
		mux.Handle("GET /metrics", cfg.Metrics.Handler())
	}

	return &OpsServer{
		srv: &http.Server{
			Addr:              cfg.Addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
	}
}

// Start serves until Shutdown is called. Errors are sent to errCh.
func (s *OpsServer) Start(errCh chan<- error) {
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
}

// Shutdown stops the listener.
func (s *OpsServer) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}
