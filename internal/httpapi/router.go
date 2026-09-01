// Package httpapi implements the ingest and management HTTP API.
package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/singha105/webhook-relay/internal/store"
)

// maxRequestBodyBytes caps any request body at 1 MiB. The largest legitimate
// body is an event whose payload is capped at 256 KiB, so this leaves generous
// room for envelope and encoding overhead while still refusing a body designed
// to exhaust memory. Enforced by http.MaxBytesReader before the JSON decoder
// allocates anything.
const maxRequestBodyBytes = 1 << 20

// Server holds the API dependencies.
type Server struct {
	store  *store.Store
	logger *slog.Logger
	cfg    ServerConfig
}

// ServerConfig is the subset of configuration the HTTP layer needs.
type ServerConfig struct {
	RequestTimeout time.Duration
}

// NewServer builds a Server.
func NewServer(st *store.Store, logger *slog.Logger, cfg ServerConfig) *Server {
	return &Server{store: st, logger: logger, cfg: cfg}
}

// Routes returns the fully wired handler.
//
// Middleware order is deliberate and reads outermost-first:
//  1. RequestID  — must be first so every later line, including a panic
//     report, carries the ID.
//  2. Recoverer  — must wrap the handlers but sit inside RequestID, so a
//     panic is logged with its request ID.
//  3. AccessLog  — inside Recoverer, so a recovered panic still produces one
//     access line with its real 500 status.
//  4. Timeout    — innermost of the cross-cutting layers; bounds handler work
//     without cutting off the logging above it.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Use(RequestID(s.logger))
	r.Use(Recoverer)
	r.Use(AccessLog)
	r.Use(s.timeout)
	r.Use(s.limitBody)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed for this resource")
	})

	// Probes live outside /v1: they are operational surface, not API surface,
	// and must not be versioned alongside it.
	r.Get("/healthz", s.healthz)
	r.Get("/readyz", s.readyz)

	r.Route("/v1", func(r chi.Router) {
		r.Route("/endpoints", func(r chi.Router) {
			r.Post("/", s.createEndpoint)
			r.Get("/", s.listEndpoints)
			r.Get("/{id}", s.getEndpoint)
			r.Patch("/{id}", s.updateEndpoint)
			r.Delete("/{id}", s.deleteEndpoint)
		})
		r.Route("/events", func(r chi.Router) {
			r.Post("/", s.ingestEvent)
			r.Get("/{id}", s.getEvent)
		})
	})

	return r
}

// timeout bounds every request. Implemented directly rather than with
// http.TimeoutHandler because that wrapper buffers the entire response in
// memory to be able to discard it on timeout, which we would rather not do on
// a path that should stay allocation-light.
func (s *Server) timeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := contextWithTimeout(r.Context(), s.cfg.RequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// limitBody caps request bodies before any decoder allocates.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
}
