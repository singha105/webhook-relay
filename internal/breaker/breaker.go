// Package breaker implements a per-endpoint circuit breaker.
//
// # What it is for
//
// An endpoint that has been returning 500 for an hour will return 500 for the
// next event too. Without a breaker every one of those events walks its full
// retry schedule — six attempts each, spread over minutes — which wastes our
// worker capacity, floods a struggling receiver with traffic it cannot serve,
// and buries the genuinely deliverable events behind a queue of doomed ones.
//
// The breaker short-circuits that: once an endpoint has failed enough times in
// a row, stop calling it, wait, then let exactly one request through to find
// out whether it has recovered.
//
// # Why the state lives in Valkey
//
// The breaker has to be shared. Ten worker goroutines across three replicas
// must agree that an endpoint is broken, or nine of them keep hammering it
// while one holds back. In-process state — the usual sync.Mutex-and-a-counter
// implementation — gives each replica its own opinion, so the effective failure
// threshold is multiplied by the replica count.
//
// endpoints.consecutive_failures remains the durable backstop. Valkey is a
// cache: flush it and every breaker silently closes. The column survives that,
// so a restarted system re-opens breakers for endpoints that were already known
// to be failing instead of stampeding them all over again.
package breaker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// State is the breaker's position for one endpoint.
type State string

const (
	// StateClosed is the healthy state: requests flow normally.
	StateClosed State = "closed"
	// StateOpen means the endpoint is failing and calls are refused outright.
	StateOpen State = "open"
	// StateHalfOpen means the cooldown has elapsed and exactly one probe is
	// admitted to test whether the endpoint has recovered.
	StateHalfOpen State = "half_open"
)

// Numeric returns a stable encoding for the metric. Prometheus gauges hold
// numbers, and a string label per state would create one series per state per
// endpoint instead of one series per endpoint.
func (s State) Numeric() float64 {
	switch s {
	case StateClosed:
		return 0
	case StateHalfOpen:
		return 1
	case StateOpen:
		return 2
	default:
		return -1
	}
}

// Defaults.
const (
	// DefaultThreshold is how many consecutive failures open the breaker.
	DefaultThreshold = 10
	// DefaultCooldown is how long it stays open before admitting a probe.
	DefaultCooldown = 5 * time.Minute
)

// ErrOpen is returned by Allow when the breaker is refusing calls.
var ErrOpen = errors.New("circuit breaker is open")

// allowScript decides atomically whether a caller may proceed.
//
// The half-open transition is the reason this is a script. When the cooldown
// expires, exactly ONE caller may probe; if several workers each read "cooldown
// elapsed" and all proceed, the recovering endpoint is hit by the whole pool at
// once — which is the stampede the breaker exists to prevent, delivered at the
// worst possible moment.
//
// SET NX on a separate probe key is what makes the winner unique.
//
// Returns {allowed, state, retry_after_ms}.
var allowScript = redis.NewScript(`
local state_key = KEYS[1]
local probe_key = KEYS[2]
local cooldown_ms = tonumber(ARGV[1])

local t      = redis.call('TIME')
local now_ms = (tonumber(t[1]) * 1000) + math.floor(tonumber(t[2]) / 1000)

local st = redis.call('HMGET', state_key, 'state', 'opened_at')
local state     = st[1]
local opened_at = tonumber(st[2])

-- No record at all means the endpoint has never failed enough to be tracked.
if state == false or state == nil then
  return {1, 'closed', 0}
end

if state == 'closed' then
  return {1, 'closed', 0}
end

if state == 'open' then
  local elapsed = now_ms - (opened_at or now_ms)
  if elapsed < cooldown_ms then
    return {0, 'open', math.ceil(cooldown_ms - elapsed)}
  end

  -- Cooldown has elapsed. Exactly one caller wins the probe slot; the probe
  -- key's TTL also bounds how long a crashed prober can block recovery.
  local won = redis.call('SET', probe_key, '1', 'NX', 'PX', 10000)
  if won then
    redis.call('HSET', state_key, 'state', 'half_open')
    return {1, 'half_open', 0}
  end
  -- Someone else is probing. Everyone else waits out the probe window rather
  -- than piling in behind them.
  return {0, 'open', 10000}
end

if state == 'half_open' then
  -- A probe is already in flight. Only the holder of the probe key proceeds.
  local won = redis.call('SET', probe_key, '1', 'NX', 'PX', 10000)
  if won then
    return {1, 'half_open', 0}
  end
  return {0, 'half_open', 10000}
end

return {1, 'closed', 0}
`)

// recordFailureScript increments the failure count and opens the breaker at the
// threshold, atomically.
//
// A read-then-write from Go would let concurrent failures each read the same
// count, so an endpoint configured to trip at 10 might take 30 failures to trip
// under a pool of workers.
//
// Returns {consecutive_failures, state}.
var recordFailureScript = redis.NewScript(`
local state_key = KEYS[1]
local probe_key = KEYS[2]
local threshold   = tonumber(ARGV[1])
local ttl_s       = tonumber(ARGV[2])
-- Recorded with the state so that any process reading this breaker computes
-- the half-open transition from the SAME cooldown the opener used, rather than
-- from its own configuration.
local cooldown_ms = tonumber(ARGV[3])

local t      = redis.call('TIME')
local now_ms = (tonumber(t[1]) * 1000) + math.floor(tonumber(t[2]) / 1000)

local state = redis.call('HGET', state_key, 'state')
local failures = redis.call('HINCRBY', state_key, 'failures', 1)

-- A failed probe re-opens immediately and restarts the cooldown. Waiting for
-- the threshold to be reached again would mean a still-broken endpoint gets
-- another full round of traffic after every single cooldown.
if state == 'half_open' then
  redis.call('HSET', state_key, 'state', 'open', 'opened_at', now_ms, 'cooldown_ms', cooldown_ms)
  redis.call('DEL', probe_key)
  redis.call('EXPIRE', state_key, ttl_s)
  return {failures, 'open'}
end

if failures >= threshold then
  if state ~= 'open' then
    redis.call('HSET', state_key, 'state', 'open', 'opened_at', now_ms, 'cooldown_ms', cooldown_ms)
  end
  redis.call('EXPIRE', state_key, ttl_s)
  return {failures, 'open'}
end

redis.call('HSET', state_key, 'state', 'closed')
redis.call('EXPIRE', state_key, ttl_s)
return {failures, 'closed'}
`)

// Breaker is a shared, per-endpoint circuit breaker.
type Breaker struct {
	client    *redis.Client
	threshold int
	cooldown  time.Duration
	enabled   bool
}

// Config configures the breaker.
type Config struct {
	Threshold int
	Cooldown  time.Duration
	Enabled   bool
}

// New builds a Breaker.
func New(client *redis.Client, cfg Config) *Breaker {
	if cfg.Threshold <= 0 {
		cfg.Threshold = DefaultThreshold
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = DefaultCooldown
	}
	return &Breaker{
		client:    client,
		threshold: cfg.Threshold,
		cooldown:  cfg.Cooldown,
		enabled:   cfg.Enabled,
	}
}

// Enabled reports whether the breaker is active.
func (b *Breaker) Enabled() bool { return b != nil && b.enabled }

// Decision is the outcome of an Allow check.
type Decision struct {
	Allowed bool
	State   State
	// RetryAfter is how long until the caller should try again. Zero when
	// allowed.
	RetryAfter time.Duration
}

// Allow reports whether a delivery to this endpoint may proceed.
//
// Fails OPEN on a Valkey error, for the same reason the rate limiter does: the
// breaker is a protection for the receiver and an optimization for us, and
// refusing every delivery because the cache is unreachable would turn a
// degraded dependency into a complete outage.
func (b *Breaker) Allow(ctx context.Context, endpointID uuid.UUID) (Decision, error) {
	if !b.Enabled() {
		return Decision{Allowed: true, State: StateClosed}, nil
	}

	res, err := allowScript.Run(ctx, b.client,
		[]string{stateKey(endpointID), probeKey(endpointID)},
		b.cooldown.Milliseconds(),
	).Slice()
	if err != nil {
		return Decision{Allowed: true, State: StateClosed}, fmt.Errorf("breaker check for endpoint %s: %w", endpointID, err)
	}
	if len(res) < 3 {
		return Decision{Allowed: true, State: StateClosed}, errors.New("breaker script returned an unexpected shape")
	}

	allowed, _ := res[0].(int64)
	state, _ := res[1].(string)
	retryMS, _ := res[2].(int64)

	return Decision{
		Allowed:    allowed == 1,
		State:      State(state),
		RetryAfter: time.Duration(retryMS) * time.Millisecond,
	}, nil
}

// RecordSuccess closes the breaker and clears the failure count.
//
// A success in any state closes it outright rather than decrementing. Half the
// point of "consecutive failures" is that one success proves the endpoint is
// serving again; decaying the count instead would keep a recovered endpoint
// one failure away from re-opening.
func (b *Breaker) RecordSuccess(ctx context.Context, endpointID uuid.UUID) error {
	if !b.Enabled() {
		return nil
	}
	pipe := b.client.TxPipeline()
	pipe.Del(ctx, stateKey(endpointID))
	pipe.Del(ctx, probeKey(endpointID))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("breaker record success for endpoint %s: %w", endpointID, err)
	}
	return nil
}

// RecordFailure increments the consecutive failure count, opening the breaker
// at the threshold.
func (b *Breaker) RecordFailure(ctx context.Context, endpointID uuid.UUID) (State, int, error) {
	if !b.Enabled() {
		return StateClosed, 0, nil
	}

	// The state key lives well past the cooldown so a slow trickle of failures
	// still accumulates, but not forever — an endpoint nobody sends to should
	// not keep a key.
	ttl := int(b.cooldown.Seconds())*4 + 3600

	res, err := recordFailureScript.Run(ctx, b.client,
		[]string{stateKey(endpointID), probeKey(endpointID)},
		b.threshold, ttl, b.cooldown.Milliseconds(),
	).Slice()
	if err != nil {
		return StateClosed, 0, fmt.Errorf("breaker record failure for endpoint %s: %w", endpointID, err)
	}
	if len(res) < 2 {
		return StateClosed, 0, errors.New("breaker failure script returned an unexpected shape")
	}

	failures, _ := res[0].(int64)
	state, _ := res[1].(string)
	return State(state), int(failures), nil
}

// Current reports an endpoint's state without changing it.
//
// Read-only, so it is safe to call from the API. It reproduces the cooldown
// arithmetic rather than calling Allow, because Allow would consume the probe
// slot — an operator refreshing a dashboard must not be the one probe that a
// recovering endpoint gets.
func (b *Breaker) Current(ctx context.Context, endpointID uuid.UUID) (State, error) {
	if !b.Enabled() {
		return StateClosed, nil
	}

	vals, err := b.client.HMGet(ctx, stateKey(endpointID), "state", "opened_at", "cooldown_ms").Result()
	if err != nil {
		return StateClosed, fmt.Errorf("breaker state for endpoint %s: %w", endpointID, err)
	}
	raw, ok := vals[0].(string)
	if !ok || raw == "" {
		return StateClosed, nil
	}

	state := State(raw)
	if state != StateOpen {
		return state, nil
	}

	openedAt, ok := vals[1].(string)
	if !ok {
		return state, nil
	}
	var openedMS int64
	if _, err := fmt.Sscanf(openedAt, "%d", &openedMS); err != nil {
		// An unparseable opened_at is not worth failing a status read over:
		// report the stored state as-is rather than returning an error that
		// would blank the field on a dashboard.
		//nolint:nilerr // deliberate: degrade to the raw state, do not fail
		return state, nil
	}
	// Prefer the cooldown recorded when the breaker opened, falling back to
	// this process's own configuration for state written by an older version.
	//
	// This matters more than it looks. Without it, Current() answers using the
	// READER's cooldown, so an API configured for 5m and a worker configured
	// for 30s report different states for the same breaker — the API says
	// "open" while the worker is already admitting probes. An operator
	// watching the dashboard would conclude the breaker was stuck.
	cooldown := b.cooldown
	if raw, ok := vals[2].(string); ok && raw != "" {
		var ms int64
		if _, err := fmt.Sscanf(raw, "%d", &ms); err == nil && ms > 0 {
			cooldown = time.Duration(ms) * time.Millisecond
		}
	}

	// Report half_open once the cooldown has elapsed, even though no probe has
	// been taken yet: that is what the breaker will do on the next call, and a
	// dashboard showing "open" for an endpoint that is about to be probed is
	// misleading.
	if time.Since(time.UnixMilli(openedMS)) >= cooldown {
		return StateHalfOpen, nil
	}
	return StateOpen, nil
}

// Reset clears an endpoint's breaker. For operator use after a fix.
func (b *Breaker) Reset(ctx context.Context, endpointID uuid.UUID) error {
	return b.RecordSuccess(ctx, endpointID)
}

func stateKey(id uuid.UUID) string { return "breaker:endpoint:" + id.String() }
func probeKey(id uuid.UUID) string { return "breaker:probe:" + id.String() }
