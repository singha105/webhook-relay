package relay_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/singha105/webhook-relay/internal/models"
	"github.com/singha105/webhook-relay/internal/queue"
	"github.com/singha105/webhook-relay/internal/relay"
	"github.com/singha105/webhook-relay/test"
)

// partialQueue enqueues the first n items and then fails.
//
// This models what a pipelined enqueue can do that a serial one could not: a
// batch that half-succeeds. Every method beyond EnqueueBatch panics, so if the
// relay ever starts depending on one, the test says so instead of silently
// passing.
type partialQueue struct {
	succeed int

	mu      sync.Mutex
	batches [][]queue.EnqueueItem
}

func (q *partialQueue) seen() [][]queue.EnqueueItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([][]queue.EnqueueItem(nil), q.batches...)
}

func (q *partialQueue) EnqueueBatch(_ context.Context, items []queue.EnqueueItem) (int, error) {
	q.mu.Lock()
	first := len(q.batches) == 0
	q.batches = append(q.batches, items)
	q.mu.Unlock()

	// Only the first call partially succeeds; after that Valkey stays down.
	//
	// This matters because the relay drains in a loop: released events are
	// immediately due again, so a fake that keeps letting a few through would
	// see them re-claimed and eventually all enqueued, and the test would be
	// asserting nothing. Staying down models the real failure — Valkey is
	// unavailable for longer than one poll tick.
	if !first {
		return 0, errors.New("valkey is still down")
	}
	if q.succeed >= len(items) {
		return len(items), nil
	}
	return q.succeed, errors.New("valkey went away mid-pipeline")
}

func (q *partialQueue) Enqueue(context.Context, uuid.UUID) error { panic("unexpected Enqueue") }
func (q *partialQueue) Claim(context.Context, string, int) ([]queue.ClaimedMessage, error) {
	panic("unexpected Claim")
}
func (q *partialQueue) Ack(context.Context, string) error { panic("unexpected Ack") }
func (q *partialQueue) Nack(context.Context, string, time.Duration) error {
	panic("unexpected Nack")
}
func (q *partialQueue) ReclaimStale(context.Context, string, time.Duration, int) ([]queue.ClaimedMessage, error) {
	panic("unexpected ReclaimStale")
}
func (q *partialQueue) Depth(context.Context) (int64, error)   { panic("unexpected Depth") }
func (q *partialQueue) Len(context.Context) (int64, error)     { panic("unexpected Len") }
func (q *partialQueue) Pending(context.Context) (int64, error) { panic("unexpected Pending") }
func (q *partialQueue) Ping(context.Context) error             { panic("unexpected Ping") }
func (q *partialQueue) Close() error                           { return nil }

// TestRelayReleasesClaimsItCouldNotEnqueue is the safety property of batching.
//
// ClaimDueEvents moves rows to 'delivering' and stamps a lease. If the enqueue
// then fails for some of them, those rows have a lease and no queue message —
// invisible to workers until the lease expires. Releasing them immediately is
// the difference between a blip and a multi-minute stall, and with a batch API
// it is easy to get wrong by treating a partial failure as total.
func TestRelayReleasesClaimsItCouldNotEnqueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := test.NewStore(t)

	ep, err := st.CreateEndpoint(ctx, "https://example.test/hook", "relay batch test", "shhh", 100)
	if err != nil {
		t.Fatalf("CreateEndpoint() = %v", err)
	}

	const total = 10
	const willSucceed = 4
	ids := make([]uuid.UUID, 0, total)
	for i := 0; i < total; i++ {
		id, err := models.NewEventID()
		if err != nil {
			t.Fatalf("NewEventID() = %v", err)
		}
		ev, _, err := st.CreateEvent(ctx, id, ep.ID, "order.created", json.RawMessage(`{"a":1}`), nil)
		if err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
		ids = append(ids, ev.ID)
	}

	q := &partialQueue{succeed: willSucceed}
	r := relay.New(st, q, slog.New(slog.NewTextHandler(io.Discard, nil)), relay.Config{BatchSize: total, PollInterval: 20 * time.Millisecond})

	batches := runOneRelayPass(t, r, q)
	if len(batches) == 0 {
		t.Fatal("the relay never enqueued anything")
	}
	if got := len(batches[0]); got != total {
		t.Errorf("first batch carried %d items, want all %d in one call", got, total)
	}

	// Every item must carry a context for its own trace; a nil one would mean
	// the batch silently shares (or loses) trace context.
	for i, it := range batches[0] {
		if it.Ctx == nil {
			t.Errorf("item %d has a nil context", i)
		}
	}

	// The 4 that made it stay claimed; the 6 that did not are released.
	var delivering, released int
	for _, id := range ids {
		ev, err := st.GetEvent(ctx, id)
		if err != nil {
			t.Fatalf("GetEvent(%s) = %v", id, err)
		}
		switch ev.Status {
		case models.StatusDelivering:
			delivering++
		case models.StatusPending, models.StatusFailed:
			released++
		default:
			t.Errorf("event %s is %q, want delivering or released", id, ev.Status)
		}
	}
	if delivering != willSucceed {
		t.Errorf("%d events left claimed, want %d (the ones actually enqueued)", delivering, willSucceed)
	}
	if released != total-willSucceed {
		t.Errorf("%d events released, want %d — the rest are stranded until their lease expires",
			released, total-willSucceed)
	}
}

// TestRelayBatchesTheWholeClaim guards the optimisation itself: one enqueue
// call per batch, not one per event. A regression here is silent — everything
// still works, just at the throughput the pipelining was meant to remove.
func TestRelayBatchesTheWholeClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := test.NewStore(t)

	ep, err := st.CreateEndpoint(ctx, "https://example.test/hook2", "relay batch count", "shhh", 100)
	if err != nil {
		t.Fatalf("CreateEndpoint() = %v", err)
	}
	const total = 25
	for i := 0; i < total; i++ {
		id, err := models.NewEventID()
		if err != nil {
			t.Fatalf("NewEventID() = %v", err)
		}
		if _, _, err := st.CreateEvent(ctx, id, ep.ID, "order.created", json.RawMessage(`{"a":1}`), nil); err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
	}

	q := &partialQueue{succeed: total}
	r := relay.New(st, q, slog.New(slog.NewTextHandler(io.Discard, nil)), relay.Config{BatchSize: total, PollInterval: 20 * time.Millisecond})

	batches := runOneRelayPass(t, r, q)
	if len(batches) != 1 {
		t.Fatalf("made %d enqueue calls for %d events, want exactly 1", len(batches), total)
	}
	if got := len(batches[0]); got != total {
		t.Errorf("batch carried %d items, want all %d", got, total)
	}
}

// runOneRelayPass starts the relay, waits for it to drain what is due, and
// stops it. Driving Run rather than the unexported relayOnce keeps this an
// external test — the test helpers import relay, so an internal test would be
// an import cycle.
func runOneRelayPass(t *testing.T, r *relay.Relay, q *partialQueue) [][]queue.EnqueueItem {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()

	deadline := time.After(15 * time.Second)
	for {
		if len(q.seen()) > 0 {
			// Give the pass a moment to finish its release work, then stop.
			time.Sleep(300 * time.Millisecond)
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("relay did not enqueue within 15s")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
	return q.seen()
}

var _ queue.Queue = (*partialQueue)(nil)
