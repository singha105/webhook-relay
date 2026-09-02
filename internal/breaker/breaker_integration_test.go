package breaker_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/singha105/webhook-relay/internal/breaker"
	"github.com/singha105/webhook-relay/test"
)

func newBreaker(t *testing.T, threshold int, cooldown time.Duration) *breaker.Breaker {
	t.Helper()
	return breaker.New(test.NewRedisClient(t), breaker.Config{
		Threshold: threshold, Cooldown: cooldown, Enabled: true,
	})
}

func TestClosedByDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBreaker(t, 3, time.Minute)
	endpoint := uuid.New()

	d, err := b.Allow(ctx, endpoint)
	if err != nil {
		t.Fatalf("Allow() = %v", err)
	}
	if !d.Allowed || d.State != breaker.StateClosed {
		t.Errorf("a never-seen endpoint = %+v, want allowed and closed", d)
	}

	state, err := b.Current(ctx, endpoint)
	if err != nil {
		t.Fatalf("Current() = %v", err)
	}
	if state != breaker.StateClosed {
		t.Errorf("Current() = %q, want closed", state)
	}
}

func TestOpensAtTheThreshold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const threshold = 5
	b := newBreaker(t, threshold, time.Minute)
	endpoint := uuid.New()

	// One short of the threshold: still closed, still delivering.
	for i := 1; i < threshold; i++ {
		state, failures, err := b.RecordFailure(ctx, endpoint)
		if err != nil {
			t.Fatalf("RecordFailure() %d = %v", i, err)
		}
		if failures != i {
			t.Errorf("failure count = %d, want %d", failures, i)
		}
		if state != breaker.StateClosed {
			t.Fatalf("breaker opened after %d failures, threshold is %d", i, threshold)
		}
		if d, _ := b.Allow(ctx, endpoint); !d.Allowed {
			t.Fatalf("calls refused after %d failures, below the threshold", i)
		}
	}

	// The threshold failure trips it.
	state, failures, err := b.RecordFailure(ctx, endpoint)
	if err != nil {
		t.Fatalf("RecordFailure() = %v", err)
	}
	if failures != threshold || state != breaker.StateOpen {
		t.Fatalf("after %d failures: state=%q failures=%d, want open/%d", threshold, state, failures, threshold)
	}

	d, err := b.Allow(ctx, endpoint)
	if err != nil {
		t.Fatalf("Allow() = %v", err)
	}
	if d.Allowed {
		t.Error("an open breaker allowed a call")
	}
	if d.RetryAfter <= 0 {
		t.Error("an open breaker gave no RetryAfter, so the caller cannot reschedule")
	}
}

func TestSuccessClosesAndResetsTheCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const threshold = 4
	b := newBreaker(t, threshold, time.Minute)
	endpoint := uuid.New()

	for i := 0; i < threshold-1; i++ {
		if _, _, err := b.RecordFailure(ctx, endpoint); err != nil {
			t.Fatalf("RecordFailure() = %v", err)
		}
	}

	if err := b.RecordSuccess(ctx, endpoint); err != nil {
		t.Fatalf("RecordSuccess() = %v", err)
	}

	// The counter must reset outright, not decay. A recovered endpoint being
	// one failure from re-opening would make the breaker flap.
	_, failures, err := b.RecordFailure(ctx, endpoint)
	if err != nil {
		t.Fatalf("RecordFailure() = %v", err)
	}
	if failures != 1 {
		t.Errorf("failure count after a success = %d, want 1", failures)
	}
}

func TestHalfOpenAfterCooldownThenRecovers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const threshold = 2
	cooldown := 300 * time.Millisecond
	b := newBreaker(t, threshold, cooldown)
	endpoint := uuid.New()

	for i := 0; i < threshold; i++ {
		if _, _, err := b.RecordFailure(ctx, endpoint); err != nil {
			t.Fatalf("RecordFailure() = %v", err)
		}
	}
	if d, _ := b.Allow(ctx, endpoint); d.Allowed {
		t.Fatal("the breaker did not open")
	}

	// Still inside the cooldown.
	time.Sleep(cooldown / 3)
	if d, _ := b.Allow(ctx, endpoint); d.Allowed {
		t.Error("a call was admitted before the cooldown elapsed")
	}

	time.Sleep(cooldown)

	d, err := b.Allow(ctx, endpoint)
	if err != nil {
		t.Fatalf("Allow() = %v", err)
	}
	if !d.Allowed {
		t.Fatalf("no probe was admitted after the cooldown: %+v", d)
	}
	if d.State != breaker.StateHalfOpen {
		t.Errorf("probe state = %q, want half_open", d.State)
	}

	// The probe succeeds: the breaker closes for everyone.
	if err := b.RecordSuccess(ctx, endpoint); err != nil {
		t.Fatalf("RecordSuccess() = %v", err)
	}
	after, err := b.Allow(ctx, endpoint)
	if err != nil {
		t.Fatalf("Allow() = %v", err)
	}
	if !after.Allowed || after.State != breaker.StateClosed {
		t.Errorf("after a successful probe = %+v, want allowed and closed", after)
	}
}

// A failed probe must re-open immediately and restart the cooldown, not wait
// for the threshold again — otherwise a still-broken endpoint receives another
// full round of traffic after every cooldown.
func TestFailedProbeReopensImmediately(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const threshold = 2
	cooldown := 300 * time.Millisecond
	b := newBreaker(t, threshold, cooldown)
	endpoint := uuid.New()

	for i := 0; i < threshold; i++ {
		if _, _, err := b.RecordFailure(ctx, endpoint); err != nil {
			t.Fatalf("RecordFailure() = %v", err)
		}
	}
	time.Sleep(cooldown + 50*time.Millisecond)

	probe, err := b.Allow(ctx, endpoint)
	if err != nil {
		t.Fatalf("Allow() = %v", err)
	}
	if !probe.Allowed || probe.State != breaker.StateHalfOpen {
		t.Fatalf("probe = %+v, want allowed and half_open", probe)
	}

	state, _, err := b.RecordFailure(ctx, endpoint)
	if err != nil {
		t.Fatalf("RecordFailure() = %v", err)
	}
	if state != breaker.StateOpen {
		t.Errorf("state after a failed probe = %q, want open", state)
	}

	// And the cooldown restarts, so the next call is refused again.
	d, err := b.Allow(ctx, endpoint)
	if err != nil {
		t.Fatalf("Allow() = %v", err)
	}
	if d.Allowed {
		t.Error("a call was admitted immediately after a failed probe")
	}
}

// The reason the half-open transition is in Lua. When the cooldown expires,
// every worker in the pool checks at once. Exactly one may probe — if they all
// do, the recovering endpoint takes the whole pool at the worst moment.
func TestExactlyOneProbeIsAdmitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const (
		threshold   = 2
		concurrency = 40
	)
	cooldown := 300 * time.Millisecond
	b := newBreaker(t, threshold, cooldown)
	endpoint := uuid.New()

	for i := 0; i < threshold; i++ {
		if _, _, err := b.RecordFailure(ctx, endpoint); err != nil {
			t.Fatalf("RecordFailure() = %v", err)
		}
	}
	time.Sleep(cooldown + 50*time.Millisecond)

	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(concurrency)
	done.Add(concurrency)

	allowed := make([]bool, concurrency)
	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			d, err := b.Allow(ctx, endpoint)
			allowed[i], errs[i] = d.Allowed, err
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	admitted := 0
	for i, ok := range allowed {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if ok {
			admitted++
		}
	}
	if admitted != 1 {
		t.Errorf("%d of %d concurrent callers were admitted as the probe, want exactly 1", admitted, concurrency)
	}
}

// Concurrent failures must not require more than `threshold` of them to trip.
func TestThresholdIsExactUnderConcurrency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const (
		threshold   = 10
		concurrency = 10
	)
	b := newBreaker(t, threshold, time.Minute)
	endpoint := uuid.New()

	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = b.RecordFailure(ctx, endpoint)
		}()
	}
	wg.Wait()

	d, err := b.Allow(ctx, endpoint)
	if err != nil {
		t.Fatalf("Allow() = %v", err)
	}
	if d.Allowed {
		t.Errorf("exactly %d concurrent failures did not trip a threshold of %d — the increment was not atomic", concurrency, threshold)
	}
}

// Reading the state must not consume the probe slot: an operator refreshing a
// dashboard must not become the one probe a recovering endpoint gets.
func TestCurrentDoesNotConsumeTheProbe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const threshold = 2
	cooldown := 250 * time.Millisecond
	b := newBreaker(t, threshold, cooldown)
	endpoint := uuid.New()

	for i := 0; i < threshold; i++ {
		if _, _, err := b.RecordFailure(ctx, endpoint); err != nil {
			t.Fatalf("RecordFailure() = %v", err)
		}
	}
	if state, _ := b.Current(ctx, endpoint); state != breaker.StateOpen {
		t.Errorf("Current() = %q during cooldown, want open", state)
	}

	time.Sleep(cooldown + 50*time.Millisecond)

	// Read it several times, as a dashboard would.
	for i := 0; i < 5; i++ {
		state, err := b.Current(ctx, endpoint)
		if err != nil {
			t.Fatalf("Current() = %v", err)
		}
		if state != breaker.StateHalfOpen {
			t.Errorf("Current() = %q after cooldown, want half_open", state)
		}
	}

	// The probe must still be available to a real caller.
	d, err := b.Allow(ctx, endpoint)
	if err != nil {
		t.Fatalf("Allow() = %v", err)
	}
	if !d.Allowed {
		t.Error("Current() consumed the probe slot")
	}
}

func TestDisabledBreakerAllowsEverything(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := breaker.New(test.NewRedisClient(t), breaker.Config{Threshold: 1, Cooldown: time.Hour, Enabled: false})
	endpoint := uuid.New()

	for i := 0; i < 20; i++ {
		if _, _, err := b.RecordFailure(ctx, endpoint); err != nil {
			t.Fatalf("RecordFailure() = %v", err)
		}
	}
	d, err := b.Allow(ctx, endpoint)
	if err != nil {
		t.Fatalf("Allow() = %v", err)
	}
	if !d.Allowed {
		t.Error("a disabled breaker refused a call")
	}
}

func TestStateNumericIsStable(t *testing.T) {
	// The metric encodes state as a number; the mapping must not drift, or
	// every dashboard and alert built on it breaks silently.
	for state, want := range map[breaker.State]float64{
		breaker.StateClosed:   0,
		breaker.StateHalfOpen: 1,
		breaker.StateOpen:     2,
	} {
		if got := state.Numeric(); got != want {
			t.Errorf("%q.Numeric() = %v, want %v", state, got, want)
		}
	}
}

// Two processes configured with different cooldowns must report the SAME state
// for one breaker. Without storing the cooldown alongside the state, Current()
// answers from the reader's own configuration — so an API on a 5m cooldown says
// "open" while a worker on 30s is already admitting probes, and an operator
// watching the dashboard concludes the breaker is stuck.
func TestStateIsConsistentAcrossDifferentlyConfiguredReaders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := test.NewRedisClient(t)
	endpoint := uuid.New()

	shortCooldown := 200 * time.Millisecond
	opener := breaker.New(client, breaker.Config{Threshold: 2, Cooldown: shortCooldown, Enabled: true})
	// A second process on a much longer cooldown, as a misconfigured or
	// mid-rollout deployment would be.
	reader := breaker.New(client, breaker.Config{Threshold: 2, Cooldown: time.Hour, Enabled: true})

	for i := 0; i < 2; i++ {
		if _, _, err := opener.RecordFailure(ctx, endpoint); err != nil {
			t.Fatalf("RecordFailure() = %v", err)
		}
	}

	if state, _ := reader.Current(ctx, endpoint); state != breaker.StateOpen {
		t.Errorf("reader sees %q immediately after opening, want open", state)
	}

	time.Sleep(shortCooldown + 100*time.Millisecond)

	openerState, err := opener.Current(ctx, endpoint)
	if err != nil {
		t.Fatalf("Current() = %v", err)
	}
	readerState, err := reader.Current(ctx, endpoint)
	if err != nil {
		t.Fatalf("Current() = %v", err)
	}
	if openerState != breaker.StateHalfOpen {
		t.Errorf("the opener sees %q after its cooldown, want half_open", openerState)
	}
	if readerState != openerState {
		t.Errorf("reader sees %q but opener sees %q — the two disagree about the same breaker",
			readerState, openerState)
	}
}
