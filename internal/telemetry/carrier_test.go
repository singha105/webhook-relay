package telemetry_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/singha105/webhook-relay/internal/telemetry"
)

// withTracing installs a real (in-memory) tracer provider and the W3C
// propagator, so these tests exercise the actual propagation code rather than
// a stub.
func withTracing(t *testing.T) trace.Tracer {
	t.Helper()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("test")
}

// The property the whole design rests on: a span started from an extracted
// context belongs to the SAME trace as the one that injected it.
func TestContextSurvivesTheQueueHop(t *testing.T) {
	tracer := withTracing(t)

	// Producer side: ingest.
	producerCtx, ingestSpan := tracer.Start(context.Background(), "ingest")
	wantTraceID := ingestSpan.SpanContext().TraceID()
	wantSpanID := ingestSpan.SpanContext().SpanID()

	fields := telemetry.InjectContext(producerCtx)
	ingestSpan.End()

	if len(fields) == 0 {
		t.Fatal("InjectContext produced no fields; nothing would cross the queue")
	}
	if _, ok := fields["traceparent"]; !ok {
		t.Errorf("no traceparent field; got keys %v", keysOf(fields))
	}

	// The fields cross the queue as ordinary strings — simulate that by
	// round-tripping through a fresh map, as a stream entry would.
	onTheWire := make(map[string]string, len(fields))
	for k, v := range fields {
		onTheWire[k] = v
	}

	// Consumer side: a different process, a fresh context.
	consumerCtx := telemetry.ExtractContext(context.Background(), onTheWire)
	_, deliverSpan := tracer.Start(consumerCtx, "deliver")
	defer deliverSpan.End()

	got := deliverSpan.SpanContext()
	if got.TraceID() != wantTraceID {
		t.Errorf("trace id = %s, want %s — the delivery span started a NEW trace", got.TraceID(), wantTraceID)
	}
	if got.SpanID() == wantSpanID {
		t.Error("the delivery span reused the ingest span id; it should be a child")
	}

	parent := trace.SpanContextFromContext(consumerCtx)
	if !parent.IsRemote() {
		t.Error("the extracted parent is not marked remote; the queue hop would not render as a boundary")
	}
	if parent.SpanID() != wantSpanID {
		t.Errorf("parent span id = %s, want the ingest span %s", parent.SpanID(), wantSpanID)
	}
}

// Several attempts for one event must all join the same trace, which is what
// makes "ingest -> attempt 1 -> attempt 2 -> attempt 3" a single connected view.
func TestEveryAttemptJoinsTheSameTrace(t *testing.T) {
	tracer := withTracing(t)

	rootCtx, root := tracer.Start(context.Background(), "ingest")
	wantTraceID := root.SpanContext().TraceID()
	fields := telemetry.InjectContext(rootCtx)
	root.End()

	for attempt := 1; attempt <= 3; attempt++ {
		ctx := telemetry.ExtractContext(context.Background(), fields)
		_, span := tracer.Start(ctx, "deliver")
		if got := span.SpanContext().TraceID(); got != wantTraceID {
			t.Errorf("attempt %d is in trace %s, want %s", attempt, got, wantTraceID)
		}
		span.End()
	}
}

func TestExtractWithoutContextStartsAFreshTrace(t *testing.T) {
	tracer := withTracing(t)

	// A message enqueued before tracing was switched on carries no fields.
	// That must degrade to a new root, not panic and not produce a broken span.
	for _, fields := range []map[string]string{nil, {}, {"unrelated": "value"}} {
		ctx := telemetry.ExtractContext(context.Background(), fields)
		_, span := tracer.Start(ctx, "deliver")
		if !span.SpanContext().IsValid() {
			t.Errorf("fields %v produced an invalid span context", fields)
		}
		span.End()
	}
}

func TestMalformedTraceparentIsIgnored(t *testing.T) {
	tracer := withTracing(t)

	// A corrupted field must not take delivery down; W3C propagation is
	// specified to ignore what it cannot parse.
	ctx := telemetry.ExtractContext(context.Background(), map[string]string{
		"traceparent": "this-is-not-a-traceparent",
	})
	_, span := tracer.Start(ctx, "deliver")
	defer span.End()
	if !span.SpanContext().IsValid() {
		t.Error("a malformed traceparent produced an invalid span context")
	}
}

func TestInjectWithoutActiveSpanIsEmpty(t *testing.T) {
	withTracing(t)
	if fields := telemetry.InjectContext(context.Background()); len(fields) != 0 {
		t.Errorf("InjectContext with no active span = %v, want empty", fields)
	}
}

func TestTraceIDFrom(t *testing.T) {
	tracer := withTracing(t)

	if got := telemetry.TraceIDFrom(context.Background()); got != "" {
		t.Errorf("TraceIDFrom outside a trace = %q, want empty", got)
	}

	ctx, span := tracer.Start(context.Background(), "op")
	defer span.End()
	got := telemetry.TraceIDFrom(ctx)
	if got != span.SpanContext().TraceID().String() {
		t.Errorf("TraceIDFrom = %q, want %q", got, span.SpanContext().TraceID())
	}
}

func TestMapCarrierKeys(t *testing.T) {
	c := telemetry.MapCarrier{"a": "1", "b": "2"}
	if got := len(c.Keys()); got != 2 {
		t.Errorf("Keys() returned %d entries, want 2", got)
	}
	if c.Get("a") != "1" {
		t.Errorf("Get(a) = %q, want 1", c.Get("a"))
	}
	c.Set("c", "3")
	if c.Get("c") != "3" {
		t.Error("Set did not store the value")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
