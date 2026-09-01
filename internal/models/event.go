package models

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// EventStatus is the lifecycle state of an event.
type EventStatus string

const (
	// StatusPending means the event is persisted and awaiting a worker.
	StatusPending EventStatus = "pending"
	// StatusDelivering means a worker has claimed the event.
	StatusDelivering EventStatus = "delivering"
	// StatusDelivered means the endpoint returned 2xx.
	StatusDelivered EventStatus = "delivered"
	// StatusFailed means an attempt failed but retries remain.
	StatusFailed EventStatus = "failed"
	// StatusDLQ means retries are exhausted; the event needs human attention.
	StatusDLQ EventStatus = "dlq"
)

// Valid reports whether s is a known status. The database enforces this too,
// via a CHECK constraint — this is the fast path that keeps a bad value from
// ever reaching the driver.
func (s EventStatus) Valid() bool {
	switch s {
	case StatusPending, StatusDelivering, StatusDelivered, StatusFailed, StatusDLQ:
		return true
	}
	return false
}

const (
	maxEventTypeLen      = 128
	maxIdempotencyKeyLen = 255
	// maxPayloadBytes caps a single event body at 256 KiB. Webhook payloads are
	// notifications, not file transfers; an unbounded JSONB column is a cheap
	// way for one caller to fill the disk.
	maxPayloadBytes = 256 * 1024
)

// Event is a single webhook to be delivered to one endpoint.
type Event struct {
	// ID is a UUIDv7: the first 48 bits are a Unix millisecond timestamp, so
	// IDs sort in creation order. That gives the primary-key B-tree sequential
	// inserts at the right edge instead of the random-page churn UUIDv4 causes,
	// and it means the PK doubles as a rough time index.
	ID         uuid.UUID `json:"id"`
	EndpointID uuid.UUID `json:"endpoint_id"`
	EventType  string    `json:"event_type"`

	// Payload is stored as JSONB. We keep it as json.RawMessage in Go so the
	// caller's bytes round-trip without a marshal/unmarshal pass that would
	// reorder keys and burn CPU on the ingest path.
	Payload json.RawMessage `json:"payload"`

	Status EventStatus `json:"status"`

	// AttemptCount is how many delivery attempts have been made. Denormalized
	// from delivery_attempts so the relay's polling query does not need a
	// correlated subquery per candidate row.
	AttemptCount int `json:"attempt_count"`

	// NextRetryAt is the earliest time this event may be delivered. While the
	// event is in 'delivering' it is instead a lease expiry: the time after
	// which the worker holding it is presumed dead.
	NextRetryAt time.Time `json:"next_retry_at"`

	// IdempotencyKey is nil when the caller did not send one. Uniqueness is
	// scoped per endpoint, not globally, so two tenants can independently use
	// the key "order-123".
	IdempotencyKey *string `json:"idempotency_key,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// EventWithAttempts is the GET /v1/events/{id} response: current status plus
// the full delivery history.
type EventWithAttempts struct {
	Event
	Attempts []DeliveryAttempt `json:"attempts"`
}

// CreateEventRequest is the POST /v1/events body. The idempotency key arrives
// as a header, not a body field, so it is not represented here.
type CreateEventRequest struct {
	EndpointID string          `json:"endpoint_id"`
	EventType  string          `json:"event_type"`
	Payload    json.RawMessage `json:"payload"`
}

// Validate checks an ingest request. This runs before any database work, so a
// malformed request costs one JSON decode and nothing else.
func (r *CreateEventRequest) Validate() (uuid.UUID, error) {
	v := &ValidationError{}

	var endpointID uuid.UUID
	switch {
	case strings.TrimSpace(r.EndpointID) == "":
		v.add("endpoint_id", "is required")
	default:
		parsed, err := uuid.Parse(r.EndpointID)
		if err != nil {
			v.add("endpoint_id", "must be a valid UUID")
		} else {
			endpointID = parsed
		}
	}

	switch {
	case strings.TrimSpace(r.EventType) == "":
		v.add("event_type", "is required")
	case len(r.EventType) > maxEventTypeLen:
		v.add("event_type", "must be at most 128 characters")
	}

	switch {
	case len(r.Payload) == 0:
		v.add("payload", "is required")
	case len(r.Payload) > maxPayloadBytes:
		v.add("payload", "must be at most 256 KiB")
	case !json.Valid(r.Payload):
		v.add("payload", "must be valid JSON")
	}

	if err := v.orNil(); err != nil {
		return uuid.Nil, err
	}
	return endpointID, nil
}

// ValidateIdempotencyKey checks the optional Idempotency-Key header. An empty
// string means the header was absent, which is always allowed.
func ValidateIdempotencyKey(key string) error {
	v := &ValidationError{}
	if len(key) > maxIdempotencyKeyLen {
		v.add("Idempotency-Key", "must be at most 255 characters")
	}
	return v.orNil()
}

// NewEventID returns a time-ordered UUIDv7 for a new event.
func NewEventID() (uuid.UUID, error) {
	return uuid.NewV7()
}
