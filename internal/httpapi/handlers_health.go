package httpapi

import (
	"context"
	"net/http"
	"time"
)

// healthz is a liveness probe: it answers as long as the process can serve
// HTTP. It deliberately touches no dependency. A liveness probe that checks
// the database would restart every API pod during a database blip, turning a
// recoverable outage into a crash loop that also loses in-flight requests.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz is a readiness probe: it reports whether this instance can serve
// traffic right now, which for the API means the database is reachable. A
// failing readyz pulls the pod out of the Service's endpoints without killing
// it, so it rejoins on its own once Postgres returns.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	// Bounded independently of the request timeout: a probe that hangs is
	// indistinguishable to Kubernetes from one that fails, but it holds a
	// connection open for the whole duration.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable",
			"reason": "database unreachable",
		})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ready"})
}
