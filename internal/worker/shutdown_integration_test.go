package worker_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/singha105/webhook-relay/internal/models"
	"github.com/singha105/webhook-relay/test"
	"github.com/singha105/webhook-relay/test/sink"
)

// Shutdown must not lose an event. This drives the drain while a delivery is
// genuinely in flight — the sink holds the request open — and asserts that the
// delivery completed and was recorded rather than being abandoned.
func TestDrainWaitsForInFlightDeliveries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := test.NewPipeline(t, test.PipelineConfig{
		Concurrency:     2,
		DeliveryTimeout: 10 * time.Second,
		DrainTimeout:    8 * time.Second,
	})

	// The sink holds each request for 1.5s, so a drain started immediately
	// after the delivery begins has to actually wait for it.
	p.Sink.SetBehavior(sink.Behavior{Status: 200, Delay: sink.Duration(1500 * time.Millisecond)})

	_, event := seedEvent(t, p)

	// Wait for the request to have ARRIVED at the sink, not to have finished.
	// The record count is the wrong signal: records are written after the
	// response is decided, so waiting on it would mean the delivery is already
	// over and there is nothing for the drain to wait on.
	if !waitFor(3*time.Second, func() bool { return p.Sink.InFlight() > 0 }) {
		t.Fatal("no delivery reached the sink; nothing was in flight to test")
	}
	if p.Pool.InFlight() == 0 {
		t.Fatal("the pool reports nothing in flight while the sink is holding a request")
	}

	start := time.Now()
	if err := p.Pool.Drain(ctx); err != nil {
		t.Fatalf("Drain() = %v", err)
	}
	elapsed := time.Since(start)

	// The drain must have BLOCKED for the in-flight delivery, not returned
	// immediately.
	if elapsed < 500*time.Millisecond {
		t.Errorf("Drain() returned after %v; it did not wait for the in-flight delivery", elapsed)
	}
	if n := p.Pool.InFlight(); n != 0 {
		t.Errorf("%d deliveries still in flight after Drain returned", n)
	}

	// And the event actually completed: not lost, not left mid-flight.
	got, err := p.Store.GetEventWithAttempts(ctx, event.ID)
	if err != nil {
		t.Fatalf("GetEventWithAttempts() = %v", err)
	}
	if got.Status != models.StatusDelivered {
		t.Errorf("status after drain = %q, want delivered", got.Status)
	}
	if len(got.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1", len(got.Attempts))
	}
}

// After a drain, work that was never claimed must still be in the queue for
// another replica — stopped, not swallowed.
func TestDrainLeavesUnclaimedWorkForAnotherReplica(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := test.NewPipeline(t, test.PipelineConfig{
		Concurrency:     1,
		DeliveryTimeout: 10 * time.Second,
		DrainTimeout:    5 * time.Second,
	})
	// Slow enough that only the first event can be in flight when we drain.
	p.Sink.SetBehavior(sink.Behavior{Status: 200, Delay: sink.Duration(800 * time.Millisecond)})

	endpoint, first := seedEvent(t, p)

	// A backlog behind it.
	const backlog = 5
	ids := make([]string, 0, backlog)
	for i := 0; i < backlog; i++ {
		id, _ := models.NewEventID()
		ev, _, err := p.Store.CreateEvent(ctx, id, endpoint.ID, "queued", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
		ids = append(ids, ev.ID.String())
	}

	if !waitFor(3*time.Second, func() bool { return p.Sink.InFlight() > 0 }) {
		t.Fatal("nothing reached the sink")
	}

	if err := p.Pool.Drain(ctx); err != nil {
		t.Fatalf("Drain() = %v", err)
	}

	// The in-flight one completed.
	got, err := p.Store.GetEvent(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetEvent() = %v", err)
	}
	if got.Status != models.StatusDelivered {
		t.Errorf("the in-flight event is %q, want delivered", got.Status)
	}

	// Nothing was lost: every backlog event is still in a non-terminal state,
	// so a replacement worker will deliver it.
	lost := 0
	for _, id := range ids {
		ev, err := p.Store.GetEvent(ctx, uuidMustParse(t, id))
		if err != nil {
			t.Fatalf("GetEvent(%s) = %v", id, err)
		}
		switch ev.Status {
		case models.StatusPending, models.StatusFailed, models.StatusDelivering, models.StatusDelivered:
			// All fine — either still queued, or delivered before the drain.
		default:
			lost++
			t.Errorf("backlog event %s is %q after shutdown", id, ev.Status)
		}
	}
	if lost > 0 {
		t.Errorf("%d backlog events were lost by the shutdown", lost)
	}
}

// A drain that runs out of budget must return rather than hang, and must say
// so. The work it abandons is unacked, so it gets reclaimed — delivered again
// rather than lost.
func TestDrainRespectsItsBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	p := test.NewPipeline(t, test.PipelineConfig{
		Concurrency: 1,
		// The sink holds far longer than the drain budget allows.
		DeliveryTimeout: 20 * time.Second,
		DrainTimeout:    600 * time.Millisecond,
	})
	p.Sink.SetBehavior(sink.Behavior{Status: 200, Delay: sink.Duration(5 * time.Second)})

	seedEvent(t, p)
	if !waitFor(3*time.Second, func() bool { return p.Sink.InFlight() > 0 }) {
		t.Fatal("nothing reached the sink")
	}

	start := time.Now()
	if err := p.Pool.Drain(ctx); err != nil {
		t.Fatalf("Drain() = %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("Drain() took %v; it did not respect its %v budget", elapsed, 600*time.Millisecond)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("Drain() returned after %v, before its budget elapsed", elapsed)
	}
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
