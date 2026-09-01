package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Outcome is what a delivery attempt tells the worker to do next.
type Outcome int

const (
	// OutcomeSuccess means the endpoint accepted the event. Terminal.
	OutcomeSuccess Outcome = iota
	// OutcomeRetryable means try again later, subject to the attempt budget.
	OutcomeRetryable
	// OutcomePermanent means retrying cannot help. Straight to the DLQ,
	// without burning the remaining attempts.
	OutcomePermanent
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSuccess:
		return "success"
	case OutcomeRetryable:
		return "retryable"
	case OutcomePermanent:
		return "permanent"
	default:
		return "unknown"
	}
}

// Result is the classified outcome of one HTTP attempt.
type Result struct {
	Outcome Outcome
	// StatusCode is nil when no HTTP response was received at all — a DNS
	// failure, refused connection, or timeout. That is a different signal from
	// a 500, and the distinction survives into delivery_attempts.
	StatusCode *int
	// ResponseBody is truncated by the caller before storage.
	ResponseBody string
	// Err is the underlying error, kept for logging.
	Err error
	// ErrorMessage is the normalized, storable description of the failure.
	// Empty on success. This is what lands in delivery_attempts.error_message,
	// so it must be stable enough to group on — "request timed out" rather
	// than a message containing an ephemeral port number.
	ErrorMessage string
	// RetryAfter is a server-requested delay parsed from the Retry-After
	// header. Zero means the header was absent or unusable.
	RetryAfter time.Duration
	// Duration is how long the attempt took.
	Duration time.Duration
}

// Classify maps an HTTP status code to an outcome.
//
// The rules, and why:
//
//   - 2xx succeeds. Anything else is a failure; a 3xx is included in that,
//     because we deliberately do not follow redirects (see client.go) and an
//     endpoint answering 302 is misconfigured rather than transiently unwell.
//
//   - 4xx is permanent, with two exceptions. A 400, 401, 403, or 404 means the
//     request as constructed will never be accepted: the URL is wrong, the
//     secret is wrong, or the receiver has rejected the shape. Retrying is
//     wasted work for us and unwanted traffic for them, and it delays the
//     operator noticing.
//
//   - 408 Request Timeout and 429 Too Many Requests are the exceptions. Both
//     explicitly describe a condition that will pass: the first says the server
//     gave up waiting, the second says slow down. Treating either as permanent
//     would drop events because a receiver was momentarily busy.
//
//   - 5xx is retryable. The server is telling us it failed, not that we did.
//     501 Not Implemented is arguably permanent, but a receiver returning it
//     for a webhook route is far more likely to be a proxy in front of a
//     restarting app than a deliberate statement, so it retries with the rest.
func Classify(statusCode int) Outcome {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return OutcomeSuccess
	case statusCode == http.StatusRequestTimeout, // 408
		statusCode == http.StatusTooManyRequests: // 429
		return OutcomeRetryable
	case statusCode >= 400 && statusCode < 500:
		return OutcomePermanent
	case statusCode >= 500:
		return OutcomeRetryable
	default:
		// 1xx and 3xx. Not a success and not a documented failure; treat as
		// retryable so a genuinely odd receiver gets a second chance rather
		// than being dead-lettered on the first surprise.
		return OutcomeRetryable
	}
}

// ParseRetryAfter interprets a Retry-After header.
//
// RFC 9110 allows two forms and receivers use both: a delay in seconds, and an
// HTTP-date. Honouring it matters for 429 in particular — a receiver that has
// told us exactly when to come back and is then ignored will simply keep
// rate-limiting us, and our own backoff would be fighting theirs.
//
// The value is clamped: a receiver asking us to wait a week is not something we
// will honour, and a malformed or negative value is treated as absent so it
// cannot be used to stall a worker.
func ParseRetryAfter(header string, now time.Time, maxDelay time.Duration) time.Duration {
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(header); err == nil {
		if secs <= 0 {
			return 0
		}
		return clampDelay(time.Duration(secs)*time.Second, maxDelay)
	}
	if when, err := http.ParseTime(header); err == nil {
		d := when.Sub(now)
		if d <= 0 {
			return 0
		}
		return clampDelay(d, maxDelay)
	}
	return 0
}

func clampDelay(d, maxDelay time.Duration) time.Duration {
	if maxDelay > 0 && d > maxDelay {
		return maxDelay
	}
	return d
}

// ClassifyTransportError maps a failed round trip to an outcome.
//
// Every transport failure is retryable. A refused connection, a DNS miss, a
// TLS handshake failure, a timeout — all of them are indistinguishable, from
// here, from an endpoint that is restarting. Context cancellation is the one
// case worth naming separately, because it means WE gave up, not that they
// failed, and it should not count against the endpoint's failure record.
func ClassifyTransportError(err error) (Outcome, string) {
	switch {
	case err == nil:
		return OutcomeSuccess, ""
	case errors.Is(err, context.Canceled):
		return OutcomeRetryable, "delivery cancelled during shutdown"
	case errors.Is(err, context.DeadlineExceeded):
		return OutcomeRetryable, "request timed out"
	default:
		return OutcomeRetryable, err.Error()
	}
}

// describeStatus renders a status code for a log line or an error message.
func describeStatus(code int) string {
	return fmt.Sprintf("%d %s", code, http.StatusText(code))
}
