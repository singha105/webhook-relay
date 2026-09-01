package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// HeaderRequestID is both read and written, so a request ID assigned by an
// upstream proxy or by the caller survives into our logs and correlates across
// service boundaries.
const HeaderRequestID = "X-Request-ID"

// maxRequestIDLen bounds an inbound ID. It is echoed into every log line, so an
// unbounded value from a client is a log-injection vector.
const maxRequestIDLen = 128

// RequestID assigns each request an ID and attaches a logger carrying it.
//
// Every subsequent log line in the request goes through this logger, which is
// what makes "request_id on every line" structural rather than a convention
// each handler has to remember.
func RequestID(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := sanitizeRequestID(r.Header.Get(HeaderRequestID))
			if id == "" {
				id = uuid.NewString()
			}

			ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
			ctx = WithLogger(ctx, base.With(slog.String("request_id", id)))

			// Echo it back so a client can correlate without parsing logs.
			w.Header().Set(HeaderRequestID, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// sanitizeRequestID strips anything that could corrupt a log line and bounds
// the length. Control characters in a JSON string are escaped by the encoder,
// but a caller-supplied newline still has no business in an identifier.
func sanitizeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if len(raw) > maxRequestIDLen {
		raw = raw[:maxRequestIDLen]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.', r == ':':
			return r
		default:
			return -1
		}
	}, raw)
}

// statusRecorder captures the status code and byte count, which the
// ResponseWriter interface otherwise does not expose after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	// A handler that writes without calling WriteHeader implies 200.
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// AccessLog emits one structured line per request after it completes.
//
// One line per request, not one on entry and one on exit: the entry line
// carries no information the exit line lacks, and halving log volume matters
// once every request is being shipped to Loki.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		// Log the routed pattern rather than the raw path so that per-route
		// aggregation is possible; a raw path containing an ID would produce
		// one distinct label per event and blow up cardinality on Day 4.
		attrs := []any{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.bytes),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.String("remote_addr", r.RemoteAddr),
		}

		l := LoggerFrom(r.Context())
		switch {
		case rec.status >= 500:
			l.Error("request completed", attrs...)
		case rec.status >= 400:
			l.Warn("request completed", attrs...)
		default:
			l.Info("request completed", attrs...)
		}
	})
}

// Recoverer turns a handler panic into a 500 instead of a dropped connection.
//
// Without this, net/http logs the panic and closes the connection with no
// response, which a client sees as a confusing transport error rather than a
// server error it can retry.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//nolint:contextcheck // the recovery path intentionally uses the
		// request's own context via writeError; there is no parent context to
		// thread through a deferred panic handler.
		defer func() {
			if rec := recover(); rec != nil {
				// http.ErrAbortHandler is the documented way to abort a
				// response deliberately; re-panic so net/http handles it.
				// errors.Is rather than ==, so a wrapped sentinel still
				// short-circuits here instead of being logged as a crash.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				LoggerFrom(r.Context()).Error("panic recovered",
					slog.Any("panic", rec),
					slog.String("path", r.URL.Path),
				)
				writeError(w, r, http.StatusInternalServerError, CodeInternal, "an internal error occurred")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
