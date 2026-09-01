package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   Outcome
	}{
		{"200 OK", 200, OutcomeSuccess},
		{"201 Created", 201, OutcomeSuccess},
		{"202 Accepted", 202, OutcomeSuccess},
		{"204 No Content", 204, OutcomeSuccess},
		{"299, still 2xx", 299, OutcomeSuccess},

		// Permanent 4xx: the request as constructed will never be accepted.
		{"400 Bad Request", 400, OutcomePermanent},
		{"401 Unauthorized — wrong secret", 401, OutcomePermanent},
		{"403 Forbidden", 403, OutcomePermanent},
		{"404 Not Found — wrong URL", 404, OutcomePermanent},
		{"410 Gone", 410, OutcomePermanent},
		{"422 Unprocessable", 422, OutcomePermanent},

		// The two documented exceptions.
		{"408 Request Timeout", 408, OutcomeRetryable},
		{"429 Too Many Requests", 429, OutcomeRetryable},

		{"500 Internal Server Error", 500, OutcomeRetryable},
		{"502 Bad Gateway", 502, OutcomeRetryable},
		{"503 Service Unavailable", 503, OutcomeRetryable},
		{"504 Gateway Timeout", 504, OutcomeRetryable},

		// We do not follow redirects, so a 3xx is a misconfigured receiver.
		// Retryable rather than permanent: give an odd receiver one more go
		// instead of dead-lettering on the first surprise.
		{"301 Moved Permanently", 301, OutcomeRetryable},
		{"302 Found", 302, OutcomeRetryable},
		{"100 Continue", 100, OutcomeRetryable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.status); got != tt.want {
				t.Errorf("Classify(%d) = %s, want %s", tt.status, got, tt.want)
			}
		})
	}
}

// The single most consequential rule: a permanent failure must not burn the
// retry budget, and a retryable one must not be dropped.
func TestClassifyBoundariesOfThe4xxRule(t *testing.T) {
	for status := 400; status < 500; status++ {
		got := Classify(status)
		want := OutcomePermanent
		if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
			want = OutcomeRetryable
		}
		if got != want {
			t.Errorf("Classify(%d) = %s, want %s", status, got, want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	const maxDelay = time.Hour

	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"absent", "", 0},
		{"delay in seconds", "120", 2 * time.Minute},
		{"one second", "1", time.Second},
		{"zero is treated as absent", "0", 0},
		{"negative is treated as absent", "-30", 0},
		{"garbage is treated as absent", "soon", 0},

		// RFC 9110 permits an HTTP-date, and receivers do use it.
		{"http-date in the future", "Tue, 01 Sep 2026 12:05:00 GMT", 5 * time.Minute},
		{"http-date in the past", "Tue, 01 Sep 2026 11:00:00 GMT", 0},
		{"http-date exactly now", "Tue, 01 Sep 2026 12:00:00 GMT", 0},

		// A receiver asking us to wait a week does not get to hold a worker.
		{"absurd delay is clamped", "604800", maxDelay},
		{"absurd http-date is clamped", "Mon, 01 Mar 2027 00:00:00 GMT", maxDelay},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseRetryAfter(tt.header, now, maxDelay); got != tt.want {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

func TestClassifyTransportError(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		want    Outcome
		wantMsg string
	}{
		{"no error", nil, OutcomeSuccess, ""},
		{"timeout", context.DeadlineExceeded, OutcomeRetryable, "request timed out"},
		{"cancelled during shutdown", context.Canceled, OutcomeRetryable, "delivery cancelled during shutdown"},
		{"connection refused", errors.New("dial tcp 127.0.0.1:9: connect: connection refused"), OutcomeRetryable, "dial tcp 127.0.0.1:9: connect: connection refused"},
		{"dns failure", errors.New("lookup nope.invalid: no such host"), OutcomeRetryable, "lookup nope.invalid: no such host"},
		{"wrapped timeout", fmt.Errorf("post failed: %w", context.DeadlineExceeded), OutcomeRetryable, "request timed out"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, msg := ClassifyTransportError(tt.err)
			if got != tt.want {
				t.Errorf("outcome = %s, want %s", got, tt.want)
			}
			if msg != tt.wantMsg {
				t.Errorf("message = %q, want %q", msg, tt.wantMsg)
			}
		})
	}
}

func TestOutcomeString(t *testing.T) {
	for outcome, want := range map[Outcome]string{
		OutcomeSuccess:   "success",
		OutcomeRetryable: "retryable",
		OutcomePermanent: "permanent",
		Outcome(99):      "unknown",
	} {
		if got := outcome.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q, want %q", outcome, got, want)
		}
	}
}
