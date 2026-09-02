package ratelimit_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/singha105/webhook-relay/internal/ratelimit"
	"github.com/singha105/webhook-relay/test"
)

func newLimiter(t *testing.T) *ratelimit.Limiter {
	t.Helper()
	return ratelimit.New(test.NewRedisClient(t), 1.0, true)
}

func TestAllowSpendsTheBucketThenRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := newLimiter(t)
	endpoint := uuid.New()

	const rate = 10 // burst factor 1.0 -> capacity 10

	// A new bucket starts full, so the first `rate` calls all succeed.
	for i := 0; i < rate; i++ {
		d, err := l.Allow(ctx, endpoint, rate)
		if err != nil {
			t.Fatalf("Allow() call %d = %v", i+1, err)
		}
		if !d.Allowed {
			t.Fatalf("call %d was refused; a fresh bucket should hold %d tokens", i+1, rate)
		}
	}

	// The next one has nothing left.
	d, err := l.Allow(ctx, endpoint, rate)
	if err != nil {
		t.Fatalf("Allow() = %v", err)
	}
	if d.Allowed {
		t.Error("the bucket allowed more than its capacity")
	}
	if d.RetryAfter <= 0 {
		t.Error("a refusal carried no RetryAfter, so the caller has nothing to schedule against")
	}
	if d.RetryAfter > 2*time.Second {
		t.Errorf("RetryAfter = %v, implausible for a %d/s bucket", d.RetryAfter, rate)
	}
}

func TestBucketRefillsOverTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := newLimiter(t)
	endpoint := uuid.New()

	// A fast rate so refill is observable without a long sleep.
	const rate = 100

	// Drain until actually refused, rather than assuming exactly `rate` calls
	// empties it. Each call is a network round trip, and at 100/s the bucket
	// refills measurably while the drain loop is running — an earlier version
	// of this test spent exactly `rate` tokens and then failed because the
	// bucket was legitimately no longer empty.
	drained := false
	for i := 0; i < rate*5; i++ {
		d, err := l.Allow(ctx, endpoint, rate)
		if err != nil {
			t.Fatalf("Allow() = %v", err)
		}
		if !d.Allowed {
			drained = true
			break
		}
	}
	if !drained {
		t.Fatal("could not drain the bucket; the limiter is not limiting")
	}

	// 100/s means ~10 tokens in 100ms.
	time.Sleep(150 * time.Millisecond)

	allowed := 0
	for i := 0; i < 40; i++ {
		d, err := l.Allow(ctx, endpoint, rate)
		if err != nil {
			t.Fatalf("Allow() = %v", err)
		}
		if d.Allowed {
			allowed++
		}
	}
	if allowed < 5 {
		t.Errorf("only %d tokens refilled after 150ms at %d/s; want at least 5", allowed, rate)
	}
	// Loose upper bound: the assertion is that refill is rate-proportional,
	// not that it is exact to the millisecond.
	if allowed > 40 {
		t.Errorf("%d permits granted, more than were requested", allowed)
	}
	t.Logf("%d permits granted after a 150ms refill at %d/s", allowed, rate)
}

// The reason the check-and-decrement lives in Lua. Fifty goroutines race one
// bucket; a read-modify-write in Go would let several of them observe the same
// token and all proceed.
func TestConcurrentCallersNeverExceedCapacity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := newLimiter(t)
	endpoint := uuid.New()

	// A slow rate so that refill during the test is negligible and the bound
	// is exactly the initial capacity.
	const (
		rate        = 5
		concurrency = 50
	)

	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(concurrency)
	done.Add(concurrency)

	results := make([]bool, concurrency)
	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			d, err := l.Allow(ctx, endpoint, rate)
			results[i], errs[i] = d.Allowed, err
		}(i)
	}

	ready.Wait()
	close(start)
	done.Wait()

	granted := 0
	for i, ok := range results {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if ok {
			granted++
		}
	}

	if granted > rate {
		t.Errorf("granted %d permits from a bucket of %d — check-and-decrement was not atomic", granted, rate)
	}
	if granted == 0 {
		t.Error("granted 0 permits; the limiter is refusing everything")
	}
	t.Logf("granted %d of %d concurrent requests against a bucket of %d", granted, concurrency, rate)
}

func TestPerEndpointIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := newLimiter(t)
	busy, quiet := uuid.New(), uuid.New()

	const rate = 3
	for i := 0; i < rate+2; i++ {
		if _, err := l.Allow(ctx, busy, rate); err != nil {
			t.Fatalf("Allow() = %v", err)
		}
	}
	if d, _ := l.Allow(ctx, busy, rate); d.Allowed {
		t.Fatal("the busy endpoint is not limited")
	}

	// A second endpoint must be untouched by the first exhausting its bucket.
	d, err := l.Allow(ctx, quiet, rate)
	if err != nil {
		t.Fatalf("Allow() = %v", err)
	}
	if !d.Allowed {
		t.Error("one endpoint's limit leaked into another's bucket")
	}
}

func TestPerEndpointRateIsHonoured(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := newLimiter(t)

	// The same limiter, two endpoints, different configured rates — this is
	// what makes the day-1 rate_limit_per_sec column meaningful.
	slow, fast := uuid.New(), uuid.New()

	countAllowed := func(id uuid.UUID, rate, calls int) int {
		n := 0
		for i := 0; i < calls; i++ {
			d, err := l.Allow(ctx, id, rate)
			if err != nil {
				t.Fatalf("Allow() = %v", err)
			}
			if d.Allowed {
				n++
			}
		}
		return n
	}

	slowAllowed := countAllowed(slow, 2, 20)
	fastAllowed := countAllowed(fast, 20, 20)

	if slowAllowed > 3 {
		t.Errorf("a 2/s endpoint allowed %d of 20 immediate calls", slowAllowed)
	}
	if fastAllowed < 15 {
		t.Errorf("a 20/s endpoint allowed only %d of 20 immediate calls", fastAllowed)
	}
}

func TestDisabledLimiterAllowsEverything(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := ratelimit.New(test.NewRedisClient(t), 1.0, false)
	endpoint := uuid.New()

	for i := 0; i < 100; i++ {
		d, err := l.Allow(ctx, endpoint, 1)
		if err != nil {
			t.Fatalf("Allow() = %v", err)
		}
		if !d.Allowed {
			t.Fatalf("a disabled limiter refused call %d", i+1)
		}
	}
}

// A misconfigured rate of zero must not silently halt an endpoint forever.
func TestZeroRateIsTreatedAsUnlimited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := newLimiter(t)
	endpoint := uuid.New()

	for i := 0; i < 20; i++ {
		d, err := l.Allow(ctx, endpoint, 0)
		if err != nil {
			t.Fatalf("Allow() = %v", err)
		}
		if !d.Allowed {
			t.Fatal("a rate of 0 blocked delivery; it must mean unlimited, not none")
		}
	}
}

func TestResetRefillsTheBucket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := newLimiter(t)
	endpoint := uuid.New()

	const rate = 4
	for i := 0; i < rate+1; i++ {
		if _, err := l.Allow(ctx, endpoint, rate); err != nil {
			t.Fatalf("Allow() = %v", err)
		}
	}
	if d, _ := l.Allow(ctx, endpoint, rate); d.Allowed {
		t.Fatal("bucket should be empty")
	}

	if err := l.Reset(ctx, endpoint); err != nil {
		t.Fatalf("Reset() = %v", err)
	}

	d, err := l.Allow(ctx, endpoint, rate)
	if err != nil {
		t.Fatalf("Allow() = %v", err)
	}
	if !d.Allowed {
		t.Error("the bucket was not refilled by Reset")
	}
}

// Idle buckets must not accumulate forever: one key per endpoint ever seen is
// a slow memory leak in a store that is also the work queue.
func TestBucketKeyExpires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := test.NewRedisClient(t)
	l := ratelimit.New(client, 1.0, true)
	endpoint := uuid.New()

	if _, err := l.Allow(ctx, endpoint, 10); err != nil {
		t.Fatalf("Allow() = %v", err)
	}

	ttl, err := client.TTL(ctx, ratelimit.Key(endpoint)).Result()
	if err != nil {
		t.Fatalf("TTL() = %v", err)
	}
	if ttl <= 0 {
		t.Errorf("TTL = %v; the bucket key would live forever", ttl)
	}
	if ttl > 10*time.Minute {
		t.Errorf("TTL = %v, longer than an idle bucket needs", ttl)
	}
}
