package delivery

import (
	"math/rand/v2"
	"time"
)

// Backoff policy defaults.
const (
	// DefaultBaseDelay is the unit the exponential is built from.
	DefaultBaseDelay = time.Second
	// DefaultMaxDelay caps the ceiling. Beyond an hour a webhook is stale
	// enough that a human should be looking at it, which is what the DLQ is
	// for.
	DefaultMaxDelay = time.Hour
	// DefaultMaxAttempts is the total number of delivery attempts, including
	// the first. After this the event goes to the dead letter queue.
	DefaultMaxAttempts = 6
)

// Backoff computes retry delays with full jitter.
type Backoff struct {
	Base        time.Duration
	Max         time.Duration
	MaxAttempts int

	// rand is injectable so tests can be deterministic. nil means the global
	// source, which is fine: this is scheduling jitter, not a security
	// boundary, so a predictable sequence costs nothing.
	rand func(n int64) int64
}

// NewBackoff returns a policy, applying defaults for zero values.
func NewBackoff(base, maxDelay time.Duration, maxAttempts int) Backoff {
	if base <= 0 {
		base = DefaultBaseDelay
	}
	if maxDelay <= 0 {
		maxDelay = DefaultMaxDelay
	}
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	return Backoff{Base: base, Max: maxDelay, MaxAttempts: maxAttempts}
}

// Ceiling returns the upper bound of the delay window after `attempt` (1-based)
// has failed: min(Max, Base * 2^attempt).
//
// Exposed separately from Delay because it is the deterministic half. Full
// jitter makes any individual delay unpredictable, so the ceiling is what you
// assert on in a test and what you show an operator who is asking why a retry
// has not happened yet.
func (b Backoff) Ceiling(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Shifting past the width of the type wraps to zero or negative. Bail out
	// to Max well before that: 2^62 nanoseconds is already ~146 years.
	if attempt >= 62 {
		return b.Max
	}
	ceiling := b.Base << uint(attempt)
	// The shift itself can overflow into negative for a large Base.
	if ceiling <= 0 || ceiling > b.Max {
		return b.Max
	}
	return ceiling
}

// Delay returns how long to wait before the next attempt, given that `attempt`
// (1-based) has just failed.
//
// # Why full jitter rather than fixed or exponential-without-jitter
//
// Consider an endpoint that goes down and comes back. Every event in flight
// fails at roughly the same moment, so with a fixed delay — or with plain
// exponential backoff — every one of them retries at roughly the same moment
// too. The herd stays synchronized: the endpoint comes back up, gets hit by the
// entire backlog at once, falls over again, and the next round is just as
// synchronized as the last. Backoff without jitter does not spread load, it
// merely postpones a spike and then reproduces it, doubling the interval
// between identical stampedes.
//
// Full jitter — a uniform draw from [0, ceiling) rather than the ceiling
// itself — breaks the correlation. After the first retry round the herd is
// smeared across the whole window, and it stays smeared, because each
// subsequent draw is independent. The arrival rate at a recovering endpoint
// becomes roughly flat instead of spiky.
//
// The cost is that the mean delay is halved (E[U(0,c)] = c/2), so a client
// retries sooner on average than the ceiling suggests, and an individual
// sequence of delays is not monotonically increasing even though the ceilings
// are. "Equal jitter" (c/2 + U(0, c/2)) trades some of the decorrelation back
// for a tighter lower bound; full jitter is chosen here because this service's
// failure mode is precisely the synchronized-herd one, and because Day 5 is
// going to create exactly that scenario on purpose.
func (b Backoff) Delay(attempt int) time.Duration {
	ceiling := b.Ceiling(attempt)
	if ceiling <= 0 {
		return 0
	}
	n := b.randN(int64(ceiling))
	return time.Duration(n)
}

func (b Backoff) randN(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if b.rand != nil {
		return b.rand(n)
	}
	return rand.Int64N(n)
}

// ShouldRetry reports whether another attempt is permitted after `attempt`
// (1-based) has failed.
func (b Backoff) ShouldRetry(attempt int) bool {
	return attempt < b.MaxAttempts
}
