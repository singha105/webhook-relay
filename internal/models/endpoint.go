package models

import (
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	maxEndpointURLLen    = 2048
	maxDescriptionLen    = 512
	maxRateLimitPerSec   = 1000
	defaultRateLimitPerS = 10
)

// Endpoint is a customer-registered destination for webhook deliveries.
type Endpoint struct {
	ID          uuid.UUID `json:"id"`
	URL         string    `json:"url"`
	Description string    `json:"description"`

	// SigningSecret is deliberately omitted from JSON. It is returned exactly
	// once, in the CreateEndpointResponse, and never appears in any subsequent
	// GET. The worker needs the plaintext to compute an HMAC at delivery time,
	// so unlike a password this cannot be stored as a one-way hash — "shown
	// once" is an API-surface guarantee, not a storage guarantee.
	SigningSecret string `json:"-"`

	IsActive        bool `json:"is_active"`
	RateLimitPerSec int  `json:"rate_limit_per_sec"`

	// ConsecutiveFailures backs the Day 3 circuit breaker. It is carried from
	// Day 1 so that opening the breaker later is a code change, not a schema
	// migration against a table that already has production rows.
	ConsecutiveFailures int `json:"consecutive_failures"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// CircuitBreakerState is populated by the API from the shared breaker
	// state in Valkey, not from a column. It is a live operational fact rather
	// than stored data — omitted when unknown so a client can tell "closed"
	// from "we could not ask".
	CircuitBreakerState string `json:"circuit_breaker_state,omitempty"`
}

// CreateEndpointRequest is the POST /v1/endpoints body.
type CreateEndpointRequest struct {
	URL         string `json:"url"`
	Description string `json:"description"`
	// RateLimitPerSec is a pointer so we can tell "field omitted" (use the
	// default) apart from an explicit 0 (which is invalid and must be rejected).
	RateLimitPerSec *int `json:"rate_limit_per_sec"`
}

// CreateEndpointResponse embeds the endpoint and adds the one-time secret.
type CreateEndpointResponse struct {
	Endpoint
	SigningSecretOnce string `json:"signing_secret"`
}

// UpdateEndpointRequest is the PATCH /v1/endpoints/{id} body. Every field is a
// pointer: a nil field means "leave this alone", which is what makes the verb
// a genuine PATCH rather than a PUT that silently blanks omitted columns.
type UpdateEndpointRequest struct {
	URL             *string `json:"url"`
	Description     *string `json:"description"`
	IsActive        *bool   `json:"is_active"`
	RateLimitPerSec *int    `json:"rate_limit_per_sec"`
}

// Validate checks a create request and normalizes the rate limit.
func (r *CreateEndpointRequest) Validate() error {
	v := &ValidationError{}
	validateURL(v, "url", r.URL)

	if len(r.Description) > maxDescriptionLen {
		v.add("description", "must be at most 512 characters")
	}
	if r.RateLimitPerSec == nil {
		d := defaultRateLimitPerS
		r.RateLimitPerSec = &d
	} else {
		validateRateLimit(v, *r.RateLimitPerSec)
	}
	return v.orNil()
}

// Validate checks a patch request. An entirely empty patch is an error rather
// than a silent no-op, because it is almost always a client bug.
func (r *UpdateEndpointRequest) Validate() error {
	v := &ValidationError{}

	if r.URL == nil && r.Description == nil && r.IsActive == nil && r.RateLimitPerSec == nil {
		v.add("body", "at least one field must be provided")
		return v.orNil()
	}
	if r.URL != nil {
		validateURL(v, "url", *r.URL)
	}
	if r.Description != nil && len(*r.Description) > maxDescriptionLen {
		v.add("description", "must be at most 512 characters")
	}
	if r.RateLimitPerSec != nil {
		validateRateLimit(v, *r.RateLimitPerSec)
	}
	return v.orNil()
}

func validateRateLimit(v *ValidationError, n int) {
	if n < 1 || n > maxRateLimitPerSec {
		v.add("rate_limit_per_sec", "must be between 1 and 1000")
	}
}

// validateURL enforces that a destination is an absolute http(s) URL with a
// host. We accept loopback and private addresses: this service is run locally
// and against test harnesses, and a blanket SSRF block would make the demo
// impossible. A hosted deployment would need an egress allowlist here — noted
// as a known gap in the README rather than half-solved.
func validateURL(v *ValidationError, field, raw string) {
	if strings.TrimSpace(raw) == "" {
		v.add(field, "is required")
		return
	}
	if len(raw) > maxEndpointURLLen {
		v.add(field, "must be at most 2048 characters")
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		v.add(field, "must be a valid URL")
		return
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		v.add(field, "must use http or https scheme")
	}
	if u.Host == "" {
		v.add(field, "must include a host")
	}
}
