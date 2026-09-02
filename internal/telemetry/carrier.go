package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// MapCarrier adapts a plain string map to OpenTelemetry's TextMapCarrier.
//
// This is the whole trick behind tracing across the queue. OpenTelemetry's
// propagators are defined over a "text map" — abstractly, a set of string
// key/value pairs — precisely so that context can travel over anything, not
// just HTTP headers. A Valkey stream entry is a set of string field/value
// pairs, so it is already the right shape.
//
// Without this, an event's trace would end at ingest and a completely separate,
// unconnected trace would start at delivery. You would be able to see that
// ingest happened and that a delivery happened, and have no way to tell that
// they were the same event — which is exactly the question you are asking when
// something is wrong.
type MapCarrier map[string]string

// Get returns the value for a key.
func (c MapCarrier) Get(key string) string { return c[key] }

// Set stores a key/value pair.
func (c MapCarrier) Set(key, value string) { c[key] = value }

// Keys lists the carried keys.
func (c MapCarrier) Keys() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}

// Compile-time check that we satisfy the interface the propagator expects.
var _ propagation.TextMapCarrier = MapCarrier{}

// InjectContext serializes the active span context into a map suitable for
// carrying on a queue message.
//
// Returns the W3C traceparent (and tracestate, when present) as ordinary
// string fields. If there is no active span the map comes back empty, which is
// harmless — Extract on the other side then simply starts a new root.
func InjectContext(ctx context.Context) map[string]string {
	carrier := MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier
}

// ExtractContext rebuilds a span context from queue message fields.
//
// The returned context carries the ORIGINAL trace ID, so a span started from it
// joins the trace that began at ingest rather than starting a new one. The
// resulting parent is marked remote, which is what makes Tempo draw the queue
// hop as a boundary rather than as an ordinary function call.
func ExtractContext(ctx context.Context, fields map[string]string) context.Context {
	if len(fields) == 0 {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, MapCarrier(fields))
}
