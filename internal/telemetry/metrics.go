package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// deliveryBuckets are the histogram boundaries for delivery latency, in
// seconds.
//
// Chosen for what we actually need to answer rather than by a default
// power-of-two ladder. The interesting region for a webhook is 10ms to ~2s:
// that is where a healthy receiver lives, and where p95 moving from 200ms to
// 800ms is a real regression worth alerting on. Above 2s the only question is
// "is it about to time out", so the buckets widen. The final boundary sits at
// the 10s delivery timeout, so the +Inf bucket contains exactly the requests
// that timed out — which makes timeouts countable from the histogram alone.
var deliveryBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10,
}

// Metrics holds every instrument the service records.
type Metrics struct {
	// Registry is exposed so the HTTP handler can serve it.
	Registry *prometheus.Registry

	eventsIngested   metric.Int64Counter
	deliveryAttempts metric.Int64Counter
	deliveryDuration metric.Float64Histogram
	eventsDLQ        metric.Int64Counter
	rateLimited      metric.Int64Counter

	// Observable gauges are read from callbacks at scrape time rather than
	// pushed, because their value is a property of the system right now, not
	// an event that happened. Sampling XLEN on a timer and caching it would
	// mean the dashboard shows a queue depth from up to a minute ago, which is
	// useless during an incident.
	mu           sync.RWMutex
	queueDepthFn func(context.Context) (int64, error)
	oldestAgeFn  func(context.Context) (float64, error)
	breakerFn    func(context.Context) (map[string]float64, error)
	provider     *sdkmetric.MeterProvider
	logger       *slog.Logger
}

// MetricsConfig configures metric collection.
type MetricsConfig struct {
	ServiceName    string
	ServiceVersion string
}

// SetupMetrics builds the meter provider and every instrument.
//
// Metrics are exposed for Prometheus to scrape rather than pushed through the
// collector. Pull is the right default for a service that already has an HTTP
// listener: scrape failures are themselves a signal (the `up` metric), and a
// service that cannot reach the collector still holds its current values for
// whoever can reach it.
func SetupMetrics(cfg MetricsConfig, logger *slog.Logger) (*Metrics, error) {
	registry := prometheus.NewRegistry()

	// Go runtime and process metrics come free and answer the first question
	// in most incidents: is it the app, or is it the box.
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	exporter, err := otelprom.New(
		otelprom.WithRegisterer(registry),
		// Without this the exporter appends its own unit suffixes and a
		// _total on top of names we have already chosen, producing
		// webhook_events_ingested_total_total.
		otelprom.WithoutCounterSuffixes(),
		otelprom.WithoutUnits(),
		// The target_info metric duplicates resource attributes onto a series
		// nothing queries here.
		otelprom.WithoutTargetInfo(),
		// otel_scope_name / _version / _schema_url land on EVERY series. They
		// identify the instrumentation library, which is constant for us, so
		// they add cardinality and force every dashboard query to carry an
		// ignoring() clause for no information.
		otelprom.WithoutScopeInfo(),
	)
	if err != nil {
		return nil, fmt.Errorf("create prometheus exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	))
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
		// Explicit buckets for the one histogram we care about. The SDK's
		// default ladder tops out at 10s with coarse steps below 1s, which is
		// the exact region a webhook's latency lives in.
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "webhook_delivery_duration_seconds"},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: deliveryBuckets,
				},
			},
		)),
	)

	meter := provider.Meter(ScopeName)
	m := &Metrics{Registry: registry, provider: provider, logger: logger}

	if m.eventsIngested, err = meter.Int64Counter(
		"webhook_events_ingested_total",
		metric.WithDescription("Events accepted by the ingest API."),
	); err != nil {
		return nil, fmt.Errorf("create events ingested counter: %w", err)
	}

	if m.deliveryAttempts, err = meter.Int64Counter(
		"webhook_delivery_attempts_total",
		metric.WithDescription("Delivery attempts, by response class and endpoint."),
	); err != nil {
		return nil, fmt.Errorf("create delivery attempts counter: %w", err)
	}

	if m.deliveryDuration, err = meter.Float64Histogram(
		"webhook_delivery_duration_seconds",
		metric.WithDescription("Wall time of one outbound delivery attempt."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("create delivery duration histogram: %w", err)
	}

	if m.eventsDLQ, err = meter.Int64Counter(
		"webhook_events_dlq_total",
		metric.WithDescription("Events moved to the dead letter queue."),
	); err != nil {
		return nil, fmt.Errorf("create dlq counter: %w", err)
	}

	if m.rateLimited, err = meter.Int64Counter(
		"webhook_rate_limited_total",
		metric.WithDescription("Deliveries deferred because the endpoint's rate limit was reached."),
	); err != nil {
		return nil, fmt.Errorf("create rate limited counter: %w", err)
	}

	if err := m.registerObservables(meter); err != nil {
		return nil, err
	}

	return m, nil
}

// registerObservables wires the gauges that are read at scrape time.
func (m *Metrics) registerObservables(meter metric.Meter) error {
	queueDepth, err := meter.Int64ObservableGauge(
		"webhook_queue_depth",
		metric.WithDescription("Entries in the delivery stream not yet handed to a consumer."),
	)
	if err != nil {
		return fmt.Errorf("create queue depth gauge: %w", err)
	}

	oldestAge, err := meter.Float64ObservableGauge(
		"webhook_queue_oldest_message_age_seconds",
		metric.WithDescription("Age of the oldest undelivered event. The SLO metric: this is what a backlog actually feels like to a customer."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return fmt.Errorf("create oldest message age gauge: %w", err)
	}

	breakerState, err := meter.Float64ObservableGauge(
		"webhook_circuit_breaker_state",
		metric.WithDescription("Circuit breaker state per endpoint: 0 closed, 1 half-open, 2 open."),
	)
	if err != nil {
		return fmt.Errorf("create breaker state gauge: %w", err)
	}

	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		m.mu.RLock()
		depthFn, ageFn, brkFn := m.queueDepthFn, m.oldestAgeFn, m.breakerFn
		m.mu.RUnlock()

		// A failing collector must not abort the whole scrape: one broken
		// gauge would take every other metric with it, blinding the dashboard
		// at precisely the moment something is wrong.
		if depthFn != nil {
			if v, err := depthFn(ctx); err != nil {
				m.logger.Warn("queue depth gauge unavailable", slog.Any("error", err))
			} else {
				o.ObserveInt64(queueDepth, v)
			}
		}
		if ageFn != nil {
			if v, err := ageFn(ctx); err != nil {
				m.logger.Warn("oldest message age gauge unavailable", slog.Any("error", err))
			} else {
				o.ObserveFloat64(oldestAge, v)
			}
		}
		if brkFn != nil {
			if states, err := brkFn(ctx); err != nil {
				m.logger.Warn("breaker state gauge unavailable", slog.Any("error", err))
			} else {
				for endpointID, state := range states {
					o.ObserveFloat64(breakerState, state,
						metric.WithAttributes(attribute.String("endpoint_id", endpointID)))
				}
			}
		}
		return nil
	}, queueDepth, oldestAge, breakerState)
	if err != nil {
		return fmt.Errorf("register metric callback: %w", err)
	}
	return nil
}

// SetQueueDepthSource installs the callback that reads queue depth at scrape
// time. Only the worker has a queue handle, so the api leaves this unset and
// simply does not report the gauge.
func (m *Metrics) SetQueueDepthSource(fn func(context.Context) (int64, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueDepthFn = fn
}

// SetOldestAgeSource installs the callback for the SLO gauge.
func (m *Metrics) SetOldestAgeSource(fn func(context.Context) (float64, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.oldestAgeFn = fn
}

// SetBreakerStateSource installs the callback for per-endpoint breaker state.
func (m *Metrics) SetBreakerStateSource(fn func(context.Context) (map[string]float64, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.breakerFn = fn
}

// RecordIngest counts an accepted event.
func (m *Metrics) RecordIngest(ctx context.Context, eventType string) {
	m.eventsIngested.Add(ctx, 1, metric.WithAttributes(
		attribute.String("event_type", eventType),
	))
}

// RecordDeliveryAttempt counts an attempt and records its duration.
//
// statusClass is a coarse bucket — 2xx, 4xx, 5xx, error — rather than the exact
// code. A label per distinct status code would multiply the series count for
// no analytical gain: nobody alerts on "503 specifically", they alert on "5xx
// rate is up".
func (m *Metrics) RecordDeliveryAttempt(ctx context.Context, statusClass, endpointID string, seconds float64) {
	attrs := metric.WithAttributes(
		attribute.String("status_class", statusClass),
		attribute.String("endpoint_id", endpointID),
	)
	m.deliveryAttempts.Add(ctx, 1, attrs)
	// The histogram deliberately does NOT carry endpoint_id. Buckets multiply:
	// 12 buckets times the endpoint count would dominate the series budget,
	// and latency is a property of the system worth watching in aggregate.
	m.deliveryDuration.Record(ctx, seconds, metric.WithAttributes(
		attribute.String("status_class", statusClass),
	))
}

// RecordDLQ counts an event that exhausted its budget or failed permanently.
func (m *Metrics) RecordDLQ(ctx context.Context, endpointID, reason string) {
	m.eventsDLQ.Add(ctx, 1, metric.WithAttributes(
		attribute.String("endpoint_id", endpointID),
		attribute.String("reason", reason),
	))
}

// RecordRateLimited counts a delivery deferred by the rate limiter.
func (m *Metrics) RecordRateLimited(ctx context.Context, endpointID string) {
	m.rateLimited.Add(ctx, 1, metric.WithAttributes(
		attribute.String("endpoint_id", endpointID),
	))
}

// Handler serves the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{
		// Exemplars attach a trace ID to a histogram bucket observation, which
		// is what lets someone click a latency spike in Grafana and land on the
		// trace that caused it. Without this the exposition drops them.
		EnableOpenMetrics: true,
		ErrorHandling:     promhttp.ContinueOnError,
	})
}

// Shutdown flushes and stops the meter provider.
func (m *Metrics) Shutdown(ctx context.Context) error {
	if m.provider == nil {
		return nil
	}
	return m.provider.Shutdown(ctx)
}
