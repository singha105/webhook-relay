// Package ratelimit implements a per-endpoint token bucket in Valkey.
//
// # Why a Lua script rather than Go code around GET/SET
//
// A token bucket is read-modify-write: read the current tokens and timestamp,
// refill by the elapsed time, decrement if there are enough, write back. Doing
// that from Go means several round trips with a gap in between, and every
// worker goroutine in every replica is racing the same key. Two workers can
// both read "1 token left" and both decide they may proceed, so an endpoint
// configured for 10/s quietly receives 20.
//
// WATCH/MULTI/EXEC would make it correct by aborting on conflict, but under
// contention — which is exactly when a rate limiter matters — it degrades into
// a retry storm against Valkey.
//
// A Lua script runs atomically on the server: no other command interleaves, so
// check-and-decrement is a single indivisible operation and one round trip.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// tokenBucketScript is the atomic check-and-decrement.
//
// Time comes from redis.call('TIME'), not from the caller. Every worker
// replica has its own clock, and a bucket refilled against a skewed clock is
// worse than no limiter at all: a worker running two seconds fast would credit
// itself two extra seconds of tokens on every call and effectively bypass the
// limit. The server's clock is the one thing all callers agree on.
//
// Returns {allowed, retry_after_ms, tokens_remaining_millis}.
var tokenBucketScript = redis.NewScript(`
local key    = KEYS[1]
local rate   = tonumber(ARGV[1])   -- tokens per second
local burst  = tonumber(ARGV[2])   -- bucket capacity
local want   = tonumber(ARGV[3])   -- tokens requested

-- TIME returns {seconds, microseconds}. Milliseconds is plenty of resolution
-- for a limiter measured in requests per second, and keeps the arithmetic
-- inside the range where Lua's doubles are exact.
local t      = redis.call('TIME')
local now_ms = (tonumber(t[1]) * 1000) + math.floor(tonumber(t[2]) / 1000)

local state  = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts     = tonumber(state[2])

if tokens == nil or ts == nil then
  -- A new bucket starts full. Starting it empty would rate-limit the first
  -- request to every endpoint, which is a surprising cold-start penalty.
  tokens = burst
  ts     = now_ms
end

-- Refill. Clamped at zero because a clock that steps backwards must not
-- produce a negative elapsed time and drain the bucket.
local elapsed_ms = now_ms - ts
if elapsed_ms < 0 then elapsed_ms = 0 end

tokens = tokens + (elapsed_ms * rate / 1000.0)
if tokens > burst then tokens = burst end

local allowed = 0
local retry_after_ms = 0

if tokens >= want then
  tokens = tokens - want
  allowed = 1
else
  -- How long until enough tokens accumulate. Returned so the caller reschedules
  -- for roughly when capacity exists, instead of polling.
  local deficit = want - tokens
  retry_after_ms = math.ceil((deficit * 1000.0) / rate)
end

redis.call('HSET', key, 'tokens', tokens, 'ts', now_ms)

-- Expire idle buckets. The TTL is the time to refill from empty plus slack, so
-- a bucket that has aged out is indistinguishable from a full one — which is
-- exactly what it would have been. Without this, every endpoint ever seen
-- would leave a key behind forever.
local ttl_s = math.ceil(burst / rate) + 60
redis.call('EXPIRE', key, ttl_s)

return {allowed, retry_after_ms, math.floor(tokens * 1000)}
`)

// Decision is the outcome of one rate-limit check.
type Decision struct {
	// Allowed reports whether the caller may proceed.
	Allowed bool
	// RetryAfter is how long until a token is expected to be available. Zero
	// when Allowed.
	RetryAfter time.Duration
	// TokensRemaining is the bucket level after the call, for diagnostics.
	TokensRemaining float64
}

// Limiter is a per-endpoint token bucket.
type Limiter struct {
	client *redis.Client
	// burstFactor multiplies the per-second rate to get bucket capacity.
	// 1.0 means one second of traffic may arrive at once.
	burstFactor float64
	enabled     bool
}

// DefaultBurstFactor allows one second of capacity to be spent in a burst.
//
// A larger factor smooths over the natural clumping of a worker pool that
// claims in batches; a much larger one stops the limiter being a rate limit at
// all, since a receiver would see the whole burst instantly.
const DefaultBurstFactor = 1.0

// New builds a Limiter. enabled=false makes every check allow, which keeps the
// call sites free of conditionals.
func New(client *redis.Client, burstFactor float64, enabled bool) *Limiter {
	if burstFactor <= 0 {
		burstFactor = DefaultBurstFactor
	}
	return &Limiter{client: client, burstFactor: burstFactor, enabled: enabled}
}

// Enabled reports whether the limiter is active.
func (l *Limiter) Enabled() bool { return l != nil && l.enabled }

// Allow performs one atomic check-and-decrement for an endpoint.
//
// On a Valkey error it FAILS OPEN — the delivery proceeds. A rate limiter is a
// courtesy to the receiver, not a correctness guarantee for us; refusing to
// deliver anything because the limiter is unreachable would turn a degraded
// dependency into a total outage. The receiver's own protections still apply,
// and this is logged so the failure is visible rather than silent.
func (l *Limiter) Allow(ctx context.Context, endpointID uuid.UUID, ratePerSec int) (Decision, error) {
	if !l.Enabled() {
		return Decision{Allowed: true}, nil
	}
	if ratePerSec <= 0 {
		// Treated as unlimited rather than as "block everything". A zero here
		// would mean a misconfiguration silently halts an endpoint forever.
		return Decision{Allowed: true}, nil
	}

	burst := float64(ratePerSec) * l.burstFactor
	if burst < 1 {
		burst = 1
	}

	res, err := tokenBucketScript.Run(ctx, l.client,
		[]string{Key(endpointID)},
		ratePerSec, burst, 1,
	).Slice()
	if err != nil {
		return Decision{Allowed: true}, fmt.Errorf("rate limit check for endpoint %s: %w", endpointID, err)
	}

	decision, err := parseDecision(res)
	if err != nil {
		return Decision{Allowed: true}, fmt.Errorf("rate limit check for endpoint %s: %w", endpointID, err)
	}
	return decision, nil
}

func parseDecision(res []any) (Decision, error) {
	if len(res) < 3 {
		return Decision{}, errors.New("token bucket script returned an unexpected shape")
	}
	allowed, ok := res[0].(int64)
	if !ok {
		return Decision{}, errors.New("token bucket script returned a non-integer allowed flag")
	}
	retryMS, ok := res[1].(int64)
	if !ok {
		return Decision{}, errors.New("token bucket script returned a non-integer retry_after")
	}
	tokensMilli, ok := res[2].(int64)
	if !ok {
		return Decision{}, errors.New("token bucket script returned a non-integer token count")
	}
	return Decision{
		Allowed:         allowed == 1,
		RetryAfter:      time.Duration(retryMS) * time.Millisecond,
		TokensRemaining: float64(tokensMilli) / 1000.0,
	}, nil
}

// Key returns the Valkey key for an endpoint's bucket.
func Key(endpointID uuid.UUID) string {
	return "ratelimit:endpoint:" + endpointID.String()
}

// Reset clears an endpoint's bucket. Used by tests and by an operator who has
// just raised a limit and does not want to wait for the old bucket to age out.
func (l *Limiter) Reset(ctx context.Context, endpointID uuid.UUID) error {
	if !l.Enabled() {
		return nil
	}
	if err := l.client.Del(ctx, Key(endpointID)).Err(); err != nil {
		return fmt.Errorf("reset rate limit bucket for %s: %w", endpointID, err)
	}
	return nil
}
