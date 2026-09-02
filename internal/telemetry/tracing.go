// Package telemetry wires OpenTelemetry tracing and metrics.
//
// Traces go to the collector over OTLP; metrics are exposed at /metrics for
// Prometheus to scrape. Both are optional: if the collector is unreachable the
// service runs normally with tracing disabled, because observability failing
// must never take delivery down with it.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ScopeName identifies our instrumentation in the trace and metric output.
const ScopeName = "github.com/singha105/webhook-relay"

// TracingConfig configures trace export.
type TracingConfig struct {
	// Enabled turns tracing on. When false, a no-op provider is installed so
	// every call site works unchanged.
	Enabled bool
	// Endpoint is the collector's OTLP gRPC address, host:port.
	Endpoint string
	// ServiceName distinguishes the api from the worker in Tempo.
	ServiceName string
	// ServiceVersion is recorded on every span.
	ServiceVersion string
	// SampleRatio is the head sampling ratio, 0..1. 1 means sample everything,
	// which is correct at this volume and for a demonstration; a production
	// deployment at real traffic would lower it.
	SampleRatio float64
}

// Tracing holds a configured tracer provider.
type Tracing struct {
	provider *sdktrace.TracerProvider
	Tracer   trace.Tracer
}

// SetupTracing installs a global tracer provider and propagator.
//
// The propagator is W3C TraceContext plus Baggage. TraceContext is what makes
// the trace survive the queue hop — the traceparent header format is a string,
// so it can be carried in a stream entry field exactly as it would be in an
// HTTP header.
//
// A failure to reach the collector is NOT fatal. The exporter connects lazily
// and retries in the background, so a collector that is down at boot simply
// means spans are dropped until it comes back.
func SetupTracing(ctx context.Context, cfg TracingConfig, logger *slog.Logger) (*Tracing, func(context.Context) error, error) {
	// The propagator is installed even when tracing is disabled. Otherwise the
	// producer side would silently stop injecting context, and turning tracing
	// on later would produce disconnected traces until every process restarted.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if !cfg.Enabled {
		logger.Info("tracing disabled")
		otel.SetTracerProvider(noop.NewTracerProvider())
		return &Tracing{Tracer: noop.NewTracerProvider().Tracer(ScopeName)},
			func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		// The collector is a sidecar or an in-cluster service; TLS between it
		// and the app would mean managing certificates for a hop that never
		// leaves the trust boundary.
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(5*time.Second),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create otlp trace exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	))
	if err != nil {
		return nil, nil, fmt.Errorf("build otel resource: %w", err)
	}

	ratio := cfg.SampleRatio
	if ratio <= 0 {
		ratio = 1.0
	}

	provider := sdktrace.NewTracerProvider(
		// ParentBased so a sampling decision made at ingest is honoured by the
		// worker. Without it the two halves of an event's life would be
		// sampled independently and a trace could contain the ingest span but
		// not the delivery, which is precisely the join we care about.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(2*time.Second),
			sdktrace.WithMaxExportBatchSize(512),
		),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)

	logger.Info("tracing enabled",
		slog.String("collector", cfg.Endpoint),
		slog.String("service", cfg.ServiceName),
		slog.Float64("sample_ratio", ratio),
	)

	t := &Tracing{provider: provider, Tracer: provider.Tracer(ScopeName)}
	return t, provider.Shutdown, nil
}

// Attribute helpers, so key names cannot drift between call sites.
var (
	AttrEventID    = func(id string) attribute.KeyValue { return attribute.String("webhook.event_id", id) }
	AttrEndpointID = func(id string) attribute.KeyValue { return attribute.String("webhook.endpoint_id", id) }
	AttrEventType  = func(t string) attribute.KeyValue { return attribute.String("webhook.event_type", t) }
	AttrAttempt    = func(n int) attribute.KeyValue { return attribute.Int("webhook.attempt", n) }
	AttrOutcome    = func(o string) attribute.KeyValue { return attribute.String("webhook.outcome", o) }
	AttrStatusCode = func(c int) attribute.KeyValue { return attribute.Int("http.response.status_code", c) }
	AttrMessageID  = func(id string) attribute.KeyValue { return attribute.String("messaging.message.id", id) }
)

// TraceIDFrom returns the current trace ID, or "" outside a sampled trace.
// Used to stamp log lines so a log in Loki can be pivoted to its trace in
// Tempo — the single most useful thing an operator does at 3am.
func TraceIDFrom(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
