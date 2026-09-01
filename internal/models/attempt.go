package models

import (
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// MaxResponseBodyBytes caps how much of an endpoint's response we retain.
// 2 KiB is enough to show an operator the error a server returned without
// letting a chatty endpoint turn our attempts table into a log sink.
const MaxResponseBodyBytes = 2 * 1024

// DeliveryAttempt records one HTTP call to an endpoint. Rows are append-only:
// the attempt history is the audit trail for at-least-once delivery, so
// nothing here is ever updated in place.
type DeliveryAttempt struct {
	ID      uuid.UUID `json:"id"`
	EventID uuid.UUID `json:"event_id"`

	// AttemptNumber starts at 1 and increases monotonically per event.
	AttemptNumber int `json:"attempt_number"`

	// StatusCode is nil when the request never produced an HTTP response —
	// a DNS failure, a refused connection, or a timeout. Distinguishing that
	// from a real 5xx matters for the Day 3 retry policy.
	StatusCode *int `json:"status_code,omitempty"`

	ResponseBody string  `json:"response_body,omitempty"`
	ErrorMessage *string `json:"error_message,omitempty"`

	DurationMS  int       `json:"duration_ms"`
	AttemptedAt time.Time `json:"attempted_at"`
}

// TruncateResponseBody clips a response body to MaxResponseBodyBytes on a rune
// boundary, so a truncated multi-byte character can never produce invalid UTF-8
// that Postgres would reject on insert.
func TruncateResponseBody(body string) string {
	if len(body) <= MaxResponseBodyBytes {
		return body
	}
	truncated := body[:MaxResponseBodyBytes]
	// Walk back off a partial multi-byte sequence. A valid rune is at most 4
	// bytes, so this loop runs at most three times.
	for len(truncated) > 0 {
		r, size := utf8.DecodeLastRuneInString(truncated)
		if r == utf8.RuneError && size <= 1 {
			truncated = truncated[:len(truncated)-1]
			continue
		}
		break
	}
	return truncated
}
