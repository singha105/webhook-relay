package httpapi

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// ctxKey is unexported so no other package can collide with our context keys.
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyLogger
)

// NewLogger builds the process-wide JSON logger.
//
// JSON rather than text because these lines are destined for Loki on Day 4,
// and a structured field is queryable where a formatted string is not. Writing
// to stdout, not a file: the process runs in a container, and the container
// runtime owns log collection and rotation.
func NewLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
}

// WithLogger stores a request-scoped logger in the context.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// LoggerFrom returns the request-scoped logger, falling back to the default so
// a missing middleware degrades to unattributed logs rather than a nil panic.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// RequestIDFrom returns the current request's ID, or "" outside a request.
func RequestIDFrom(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return id
	}
	return ""
}

// contextWithTimeout is a thin indirection so the router does not import
// context solely for one call, keeping the middleware chain readable.
func contextWithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
