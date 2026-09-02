package telemetry_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/singha105/webhook-relay/internal/telemetry"
)

func newMetrics(t *testing.T) *telemetry.Metrics {
	t.Helper()
	m, err := telemetry.SetupMetrics(telemetry.MetricsConfig{
		ServiceName: "webhook-relay-test", ServiceVersion: "test",
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("SetupMetrics() = %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	return m
}

func scrape(t *testing.T, m *telemetry.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	// OpenMetrics is what carries exemplars; the default text format drops them.
	req.Header.Set("Accept", "application/openmetrics-text; version=1.0.0")
	m.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/metrics returned %d", rec.Code)
	}
	return rec.Body.String()
}

// The metric names are a contract: the dashboard JSON, the alert rules, and the
// SLO PromQL in the README all hard-code them. A rename by the exporter — an
// appended _total, a unit suffix — silently breaks every one of those.
func TestExposedMetricNamesMatchTheContract(t *testing.T) {
	m := newMetrics(t)
	ctx := context.Background()

	m.RecordIngest(ctx, "order.created")
	m.RecordDeliveryAttempt(ctx, "2xx", "11111111-1111-4111-8111-111111111111", 0.123)
	m.RecordDLQ(ctx, "11111111-1111-4111-8111-111111111111", "exhausted")
	m.RecordRateLimited(ctx, "11111111-1111-4111-8111-111111111111")
	m.SetQueueDepthSource(func(context.Context) (int64, error) { return 42, nil })
	m.SetOldestAgeSource(func(context.Context) (float64, error) { return 3.5, nil })
	m.SetBreakerStateSource(func(context.Context) (map[string]float64, error) {
		return map[string]float64{"11111111-1111-4111-8111-111111111111": 2}, nil
	})

	body := scrape(t, m)

	want := []string{
		"webhook_events_ingested_total",
		"webhook_delivery_attempts_total",
		"webhook_delivery_duration_seconds",
		"webhook_queue_depth",
		"webhook_queue_oldest_message_age_seconds",
		"webhook_events_dlq_total",
		"webhook_circuit_breaker_state",
		"webhook_rate_limited_total",
	}
	for _, name := range want {
		if !strings.Contains(body, name) {
			t.Errorf("metric %q is absent from /metrics", name)
		}
	}

	// Catch the exporter double-suffixing, which is silent and fatal to every
	// query built on these names. Scoped to our own metrics: the Go runtime
	// collector legitimately exports process_cpu_seconds_total.
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "webhook_") {
			continue
		}
		for _, bad := range []string{"_total_total", "_seconds_seconds", "_seconds_total"} {
			if strings.Contains(line, bad) {
				t.Errorf("double suffix %q in: %s", bad, line)
			}
		}
	}

	// Scope labels would land on every series and force an ignoring() clause
	// into every dashboard query.
	if strings.Contains(body, "otel_scope_name") {
		t.Error("/metrics carries otel_scope_* labels on our series")
	}
}

func TestLabelsMatchTheContract(t *testing.T) {
	m := newMetrics(t)
	ctx := context.Background()
	const endpoint = "22222222-2222-4222-8222-222222222222"

	m.RecordIngest(ctx, "order.paid")
	m.RecordDeliveryAttempt(ctx, "5xx", endpoint, 1.5)
	m.RecordRateLimited(ctx, endpoint)
	m.SetBreakerStateSource(func(context.Context) (map[string]float64, error) {
		return map[string]float64{endpoint: 1}, nil
	})

	body := scrape(t, m)

	checks := []struct{ metric, label string }{
		{"webhook_events_ingested_total", `event_type="order.paid"`},
		{"webhook_delivery_attempts_total", `status_class="5xx"`},
		{"webhook_delivery_attempts_total", `endpoint_id="` + endpoint + `"`},
		{"webhook_rate_limited_total", `endpoint_id="` + endpoint + `"`},
		{"webhook_circuit_breaker_state", `endpoint_id="` + endpoint + `"`},
	}
	for _, c := range checks {
		found := false
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, c.metric) && strings.Contains(line, c.label) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s has no series carrying %s", c.metric, c.label)
		}
	}
}

// Bucket boundaries are load-bearing: the p95 panel and the SLO both read them.
func TestHistogramBucketsCoverTheInterestingRange(t *testing.T) {
	m := newMetrics(t)
	m.RecordDeliveryAttempt(context.Background(), "2xx", "e", 0.15)

	body := scrape(t, m)
	for _, le := range []string{"0.005", "0.05", "0.25", "1.0", "10.0"} {
		if !strings.Contains(body, `le="`+le+`"`) {
			t.Errorf("histogram has no bucket boundary at %s", le)
		}
	}

	// The top boundary must sit at the delivery timeout, so +Inf minus that
	// bucket counts exactly the requests that timed out.
	if !strings.Contains(body, `le="+Inf"`) {
		t.Error("histogram has no +Inf bucket")
	}
}

// The histogram must NOT carry endpoint_id: buckets multiply, and 12 boundaries
// times the endpoint count would dominate the series budget.
func TestHistogramDoesNotCarryEndpointID(t *testing.T) {
	m := newMetrics(t)
	m.RecordDeliveryAttempt(context.Background(), "2xx", "33333333-3333-4333-8333-333333333333", 0.2)

	body := scrape(t, m)
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "webhook_delivery_duration_seconds") &&
			strings.Contains(line, "endpoint_id") {
			t.Errorf("the latency histogram carries endpoint_id, multiplying series per endpoint:\n  %s", line)
		}
	}
}

// A gauge whose source fails must not abort the scrape — one broken collector
// would blind the whole dashboard at the moment it is needed.
func TestAFailingGaugeDoesNotBreakTheScrape(t *testing.T) {
	m := newMetrics(t)
	m.RecordIngest(context.Background(), "order.created")
	m.SetQueueDepthSource(func(context.Context) (int64, error) {
		return 0, io.ErrUnexpectedEOF
	})

	body := scrape(t, m)
	if !strings.Contains(body, "webhook_events_ingested_total") {
		t.Error("a failing gauge callback suppressed unrelated metrics")
	}
}

func TestGaugesReportTheirSourceValues(t *testing.T) {
	m := newMetrics(t)
	m.SetQueueDepthSource(func(context.Context) (int64, error) { return 137, nil })
	m.SetOldestAgeSource(func(context.Context) (float64, error) { return 12.5, nil })

	body := scrape(t, m)

	depth := regexp.MustCompile(`(?m)^webhook_queue_depth(\{[^}]*\})? (\S+)$`).FindStringSubmatch(body)
	if depth == nil {
		t.Fatalf("webhook_queue_depth not found in:\n%s", body)
	}
	// OpenMetrics renders every value as a float, so 137 is exposed as "137.0".
	if depth[2] != "137" && depth[2] != "137.0" {
		t.Errorf("webhook_queue_depth = %s, want 137", depth[2])
	}

	age := regexp.MustCompile(`(?m)^webhook_queue_oldest_message_age_seconds(\{[^}]*\})? (\S+)$`).FindStringSubmatch(body)
	if age == nil {
		t.Fatalf("webhook_queue_oldest_message_age_seconds not found")
	}
	if age[2] != "12.5" {
		t.Errorf("oldest message age = %s, want 12.5", age[2])
	}
}

// Runtime metrics answer "is it the app or the box", which is the first
// question in most incidents.
func TestRuntimeMetricsArePresent(t *testing.T) {
	body := scrape(t, newMetrics(t))
	for _, name := range []string{"go_goroutines", "go_memstats_alloc_bytes"} {
		if !strings.Contains(body, name) {
			t.Errorf("runtime metric %q is absent", name)
		}
	}
}
