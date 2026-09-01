package delivery

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// DefaultDedupTTL is how long a delivery marker lives.
//
// It has to outlive the window in which a duplicate can plausibly arrive,
// which is bounded by the stale-reclaim timeout plus one delivery timeout,
// with headroom. It must NOT be indefinite: these keys are pure overhead once
// the risk has passed, and an unbounded set of them is a memory leak in a
// store we also use as a queue.
const DefaultDedupTTL = 15 * time.Minute

// Deduper prevents the same (event, attempt) pair being dispatched twice.
//
// # Why this is needed
//
// At-least-once delivery means a worker can send a request and die before
// acknowledging it. The stream entry is then reclaimed and handed to another
// worker, which has no way to know the request already went out — so the
// receiver gets it twice. Reclaim is not an edge case: it is the mechanism
// that makes the system fault-tolerant, so it fires precisely when things are
// going wrong and duplicates are least welcome.
//
// A SETNX with a TTL closes most of that window. Before dispatching, a worker
// claims the key delivery:{event_id}:{attempt}. If the key already exists,
// another worker has dispatched this exact attempt and this one must not.
//
// # What it does not do
//
// This narrows the window; it cannot close it. The gap between claiming the key
// and the request actually reaching the receiver is still unprotected: a worker
// that sets the key and dies before the HTTP call has now blocked the retry
// that should have replaced it, until the TTL expires. The reverse ordering —
// dispatch, then mark — would trade that for the duplicate we are trying to
// avoid. There is no ordering that gives exactly-once across a network
// boundary, which is why every delivery carries a stable X-Webhook-Id and the
// receiver is told to deduplicate on it.
//
// Keying on (event, attempt) rather than (event) alone is deliberate: retries
// are supposed to happen. Only a *redelivery of the same attempt* is a
// duplicate.
type Deduper struct {
	client  *redis.Client
	ttl     time.Duration
	enabled bool
}

// NewDeduper builds a Deduper.
//
// enabled is configurable so the guard can be switched off on purpose. Day 5
// disables it to demonstrate the duplicate deliveries it prevents: a control
// that is never observed failing is indistinguishable from one that does
// nothing.
func NewDeduper(client *redis.Client, ttl time.Duration, enabled bool) *Deduper {
	if ttl <= 0 {
		ttl = DefaultDedupTTL
	}
	return &Deduper{client: client, ttl: ttl, enabled: enabled}
}

// Enabled reports whether the guard is active.
func (d *Deduper) Enabled() bool { return d != nil && d.enabled }

// Claim attempts to reserve an (event, attempt) pair for dispatch.
//
// Returns true if this caller may dispatch, false if another already has.
//
// A Valkey failure returns true, not an error: the guard is an optimization
// over an at-least-once system, and refusing to deliver because the cache is
// unavailable would convert a duplicate-delivery risk into a
// no-delivery outage. Failing open here is the correct trade — the receiver is
// already required to tolerate duplicates.
func (d *Deduper) Claim(ctx context.Context, eventID uuid.UUID, attempt int) (bool, error) {
	if !d.Enabled() {
		return true, nil
	}
	ok, err := d.client.SetNX(ctx, dedupKey(eventID, attempt), "1", d.ttl).Result()
	if err != nil {
		return true, fmt.Errorf("dedup claim for event %s attempt %d: %w", eventID, attempt, err)
	}
	return ok, nil
}

// Release drops a marker.
//
// Called when a dispatch was claimed but never actually attempted — the worker
// is shutting down, or the event turned out to be gone. Without this, an
// abandoned claim would block the legitimate retry for the whole TTL.
func (d *Deduper) Release(ctx context.Context, eventID uuid.UUID, attempt int) error {
	if !d.Enabled() {
		return nil
	}
	if err := d.client.Del(ctx, dedupKey(eventID, attempt)).Err(); err != nil {
		return fmt.Errorf("dedup release for event %s attempt %d: %w", eventID, attempt, err)
	}
	return nil
}

func dedupKey(eventID uuid.UUID, attempt int) string {
	return fmt.Sprintf("delivery:%s:%d", eventID, attempt)
}
