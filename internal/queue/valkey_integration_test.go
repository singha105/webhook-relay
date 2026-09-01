package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/singha105/webhook-relay/internal/queue"
	"github.com/singha105/webhook-relay/test"
)

func TestEnqueueAndClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := test.NewQueue(t)

	t.Run("an empty queue reports ErrEmpty rather than blocking", func(t *testing.T) {
		start := time.Now()
		_, err := q.Claim(ctx, "c1", 10)
		if !errors.Is(err, queue.ErrEmpty) {
			t.Fatalf("Claim() = %v, want ErrEmpty", err)
		}
		// A blocking read would make shutdown ragged; assert it really returns.
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("Claim() blocked for %v on an empty queue", elapsed)
		}
	})

	t.Run("an enqueued event comes back", func(t *testing.T) {
		id := uuid.New()
		if err := q.Enqueue(ctx, id); err != nil {
			t.Fatalf("Enqueue() = %v", err)
		}
		msgs, err := q.Claim(ctx, "c1", 10)
		if err != nil {
			t.Fatalf("Claim() = %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("claimed %d messages, want 1", len(msgs))
		}
		if msgs[0].EventID != id {
			t.Errorf("EventID = %s, want %s", msgs[0].EventID, id)
		}
		if msgs[0].MessageID == "" {
			t.Error("MessageID is empty")
		}
		if msgs[0].DeliveryCount != 1 {
			t.Errorf("DeliveryCount = %d, want 1 on first delivery", msgs[0].DeliveryCount)
		}
		if err := q.Ack(ctx, msgs[0].MessageID); err != nil {
			t.Fatalf("Ack() = %v", err)
		}
	})

	t.Run("claim respects count", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			if err := q.Enqueue(ctx, uuid.New()); err != nil {
				t.Fatalf("Enqueue() = %v", err)
			}
		}
		msgs, err := q.Claim(ctx, "c1", 2)
		if err != nil {
			t.Fatalf("Claim() = %v", err)
		}
		if len(msgs) != 2 {
			t.Errorf("claimed %d, want 2", len(msgs))
		}
		for _, m := range msgs {
			_ = q.Ack(ctx, m.MessageID)
		}
	})
}

// The property that rules out a fan-out design: with one consumer group, each
// entry goes to exactly one worker.
func TestEachEntryGoesToExactlyOneConsumer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := test.NewQueue(t)

	const n = 30
	want := make(map[uuid.UUID]bool, n)
	for i := 0; i < n; i++ {
		id := uuid.New()
		want[id] = true
		if err := q.Enqueue(ctx, id); err != nil {
			t.Fatalf("Enqueue() = %v", err)
		}
	}

	seen := make(map[uuid.UUID]int, n)
	for _, consumer := range []string{"worker-a", "worker-b", "worker-c"} {
		for {
			msgs, err := q.Claim(ctx, consumer, 10)
			if errors.Is(err, queue.ErrEmpty) {
				break
			}
			if err != nil {
				t.Fatalf("Claim(%s) = %v", consumer, err)
			}
			for _, m := range msgs {
				seen[m.EventID]++
				_ = q.Ack(ctx, m.MessageID)
			}
		}
	}

	if len(seen) != n {
		t.Errorf("saw %d distinct events, want %d", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("event %s was delivered %d times across consumers, want 1", id, count)
		}
	}
}

// The whole reason for choosing Streams over a LIST: a consumer that claims and
// then dies must not take the work with it.
func TestReclaimStaleRecoversWorkFromADeadConsumer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := test.NewQueue(t)

	id := uuid.New()
	if err := q.Enqueue(ctx, id); err != nil {
		t.Fatalf("Enqueue() = %v", err)
	}

	// "dead-worker" claims the entry and never acks — it has been killed.
	claimed, err := q.Claim(ctx, "dead-worker", 10)
	if err != nil {
		t.Fatalf("Claim() = %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d, want 1", len(claimed))
	}

	t.Run("a fresh claim does not see it, because it is not lost, it is held", func(t *testing.T) {
		_, err := q.Claim(ctx, "live-worker", 10)
		if !errors.Is(err, queue.ErrEmpty) {
			t.Errorf("Claim() = %v, want ErrEmpty — the entry is pending, not new", err)
		}
	})

	t.Run("reclaim before the idle timeout returns nothing", func(t *testing.T) {
		_, err := q.ReclaimStale(ctx, "live-worker", time.Hour, 10)
		if !errors.Is(err, queue.ErrEmpty) {
			t.Errorf("ReclaimStale() = %v, want ErrEmpty — the entry is not stale yet", err)
		}
	})

	t.Run("reclaim after the idle timeout recovers it", func(t *testing.T) {
		// Let the entry age past a short timeout rather than sleeping for a
		// realistic one.
		time.Sleep(1200 * time.Millisecond)

		msgs, err := q.ReclaimStale(ctx, "live-worker", time.Second, 10)
		if err != nil {
			t.Fatalf("ReclaimStale() = %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("reclaimed %d, want 1", len(msgs))
		}
		if msgs[0].EventID != id {
			t.Errorf("EventID = %s, want %s", msgs[0].EventID, id)
		}
		// A redelivery must be distinguishable from a first delivery: that is
		// how an operator tells "the endpoint is failing" from "our workers
		// keep dying".
		if msgs[0].DeliveryCount < 2 {
			t.Errorf("DeliveryCount = %d, want >= 2 on a reclaimed entry", msgs[0].DeliveryCount)
		}

		if err := q.Ack(ctx, msgs[0].MessageID); err != nil {
			t.Fatalf("Ack() = %v", err)
		}
	})

	t.Run("after ack it is gone for good", func(t *testing.T) {
		_, err := q.ReclaimStale(ctx, "live-worker", time.Millisecond, 10)
		if !errors.Is(err, queue.ErrEmpty) {
			t.Errorf("ReclaimStale() = %v, want ErrEmpty after ack", err)
		}
	})
}

func TestNackMakesAnEntryReclaimable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := test.NewQueue(t)

	id := uuid.New()
	if err := q.Enqueue(ctx, id); err != nil {
		t.Fatalf("Enqueue() = %v", err)
	}
	msgs, err := q.Claim(ctx, "worker-1", 10)
	if err != nil {
		t.Fatalf("Claim() = %v", err)
	}

	if err := q.Nack(ctx, msgs[0].MessageID, time.Second); err != nil {
		t.Fatalf("Nack() = %v", err)
	}

	// Nack parks the entry under a consumer that never polls, so a reclaim
	// sweep is what picks it up.
	time.Sleep(200 * time.Millisecond)
	reclaimed, err := q.ReclaimStale(ctx, "worker-2", 100*time.Millisecond, 10)
	if err != nil {
		t.Fatalf("ReclaimStale() = %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].EventID != id {
		t.Fatalf("reclaimed %+v, want the nacked event %s", reclaimed, id)
	}
	_ = q.Ack(ctx, reclaimed[0].MessageID)
}

func TestDepthAndPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := test.NewQueue(t)

	assertCounts := func(t *testing.T, wantDepth, wantPending int64) {
		t.Helper()
		depth, err := q.Depth(ctx)
		if err != nil {
			t.Fatalf("Depth() = %v", err)
		}
		pending, err := q.Pending(ctx)
		if err != nil {
			t.Fatalf("Pending() = %v", err)
		}
		if depth != wantDepth || pending != wantPending {
			t.Errorf("depth=%d pending=%d, want depth=%d pending=%d", depth, pending, wantDepth, wantPending)
		}
	}

	assertCounts(t, 0, 0)

	for i := 0; i < 3; i++ {
		if err := q.Enqueue(ctx, uuid.New()); err != nil {
			t.Fatalf("Enqueue() = %v", err)
		}
	}
	// Enqueued but undelivered: depth 3, nothing pending.
	assertCounts(t, 3, 0)

	msgs, err := q.Claim(ctx, "w", 3)
	if err != nil {
		t.Fatalf("Claim() = %v", err)
	}
	// Claimed but unacked: depth drains, pending rises.
	assertCounts(t, 0, 3)

	for _, m := range msgs {
		if err := q.Ack(ctx, m.MessageID); err != nil {
			t.Fatalf("Ack() = %v", err)
		}
	}
	assertCounts(t, 0, 0)
}

func TestPing(t *testing.T) {
	t.Parallel()
	if err := test.NewQueue(t).Ping(context.Background()); err != nil {
		t.Errorf("Ping() = %v", err)
	}
}
