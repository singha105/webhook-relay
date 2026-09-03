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
	for i := 0; i < total; i++ {
		id, err := models.NewEventID()
		if err != nil {
			t.Fatalf("NewEventID() = %v", err)
		}
		if _, _, err := st.CreateEvent(ctx, id, ep.ID, "order.created", json.RawMessage(`{"a":1}`), nil); err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
	}

	q := &partialQueue{succeed: willSucceed}
	r := relay.New(st, q, slog.New(slog.NewTextHandler(io.Discard, nil)), relay.Config{BatchSize: total, PollInterval: 20 * time.Millisecond})

	// Wait for a second pass. This is the race-free way to prove the release
	// happened: an event that was NOT released stays in 'delivering' forever
	// and can never be claimed again, so it cannot appear in a later batch.
	// Asserting on the database instead would be a race — a released event is
	// immediately due again, so the relay re-claims it and the row oscillates.
	batches := waitForBatches(t, r, q, 2)

	if got := len(batches[0]); got != total {
		t.Fatalf("first batch carried %d items, want all %d in one call", got, total)
	}

	// Every item must carry a context for its own trace; a nil one would mean
	// the batch silently shares (or loses) trace context.
	for i, it := range batches[0] {
		if it.Ctx == nil {
			t.Errorf("item %d has a nil context", i)
		}
	}

	// The first willSucceed items were enqueued and keep their claim. The rest
	// had a lease and no message, so the relay must have released them — which
	// is exactly the set the next pass re-claims.
	wantRetried := make(map[uuid.UUID]bool, total-willSucceed)
	for _, it := range batches[0][willSucceed:] {
		wantRetried[it.EventID] = true
	}
	gotRetried := make(map[uuid.UUID]bool, len(batches[1]))
	for _, it := range batches[1] {
		gotRetried[it.EventID] = true
	}

	if len(gotRetried) != len(wantRetried) {
		t.Errorf("second pass re-claimed %d events, want %d (the ones that failed to enqueue)",
			len(gotRetried), len(wantRetried))
	}
	for id := range wantRetried {
		if !gotRetried[id] {
			t.Errorf("event %s failed to enqueue but was never re-claimed; "+
				"it is stranded in 'delivering' until its lease expires", id)
		}
	}
	// The successfully enqueued ones must NOT come back: they hold a valid
	// claim and a real queue message, so re-claiming them would double-deliver.
	for _, it := range batches[0][:willSucceed] {
		if gotRetried[it.EventID] {
			t.Errorf("event %s was enqueued successfully but got re-claimed", it.EventID)
		}
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

	batches := waitForBatches(t, r, q, 1)
	if got := len(batches[0]); got != total {
		t.Errorf("first batch carried %d items, want all %d in one call, "+
			"not one call per event", got, total)
	}
}

// waitForBatches starts the relay, waits until it has made at least n enqueue
// calls, and stops it.
//
// Driving Run rather than the unexported relayOnce keeps this an external test:
// the shared test helpers import relay, so an internal test would be an import
// cycle.
func waitForBatches(t *testing.T, r *relay.Relay, q *partialQueue, n int) [][]queue.EnqueueItem {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.After(20 * time.Second)
	for {
		if got := q.seen(); len(got) >= n {
			return got
		}
		select {
		case <-deadline:
			t.Fatalf("relay made %d enqueue calls in 20s, want %d", len(q.seen()), n)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

var _ queue.Queue = (*partialQueue)(nil)
