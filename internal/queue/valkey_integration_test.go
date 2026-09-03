package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/singha105/webhook-relay/internal/queue"
	"github.com/singha105/webhook-relay/internal/telemetry"
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

// The queue is where trace context has to survive, so assert it here rather
// than only in the telemetry package's unit tests: this exercises the real
// round trip through Valkey, including the field namespacing.
func TestTraceContextSurvivesTheStream(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := test.NewQueue(t)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	defer func() { _ = tp.Shutdown(ctx) }()
	tracer := tp.Tracer("queue-test")

	producerCtx, span := tracer.Start(ctx, "ingest")
	wantTrace := span.SpanContext().TraceID()
	wantSpan := span.SpanContext().SpanID()

	id := uuid.New()
	if err := q.Enqueue(producerCtx, id); err != nil {
		t.Fatalf("Enqueue() = %v", err)
	}
	span.End()

	msgs, err := q.Claim(ctx, "consumer", 10)
	if err != nil {
		t.Fatalf("Claim() = %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("claimed %d messages, want 1", len(msgs))
	}
	msg := msgs[0]
	defer func() { _ = q.Ack(ctx, msg.MessageID) }()

	if len(msg.TraceFields) == 0 {
		t.Fatal("the claimed message carries no trace fields; the trace would break at the queue")
	}
	if _, ok := msg.TraceFields["traceparent"]; !ok {
		t.Errorf("trace fields = %v, want a traceparent key with the prefix stripped", msg.TraceFields)
	}

	// The consumer side: a span started here must join the producer's trace.
	consumerCtx := telemetry.ExtractContext(ctx, msg.TraceFields)
	_, deliverSpan := tracer.Start(consumerCtx, "deliver")
	defer deliverSpan.End()

	if got := deliverSpan.SpanContext().TraceID(); got != wantTrace {
		t.Errorf("delivery is in trace %s, want %s — the trace broke crossing Valkey", got, wantTrace)
	}
	parent := trace.SpanContextFromContext(consumerCtx)
	if parent.SpanID() != wantSpan {
		t.Errorf("parent span = %s, want the ingest span %s", parent.SpanID(), wantSpan)
	}
}

func TestLenCountsEveryEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := test.NewQueue(t)

	if n, err := q.Len(ctx); err != nil || n != 0 {
		t.Fatalf("Len() on an empty stream = %d, %v", n, err)
	}
	for i := 0; i < 4; i++ {
		if err := q.Enqueue(ctx, uuid.New()); err != nil {
			t.Fatalf("Enqueue() = %v", err)
		}
	}
	msgs, err := q.Claim(ctx, "c", 4)
	if err != nil {
		t.Fatalf("Claim() = %v", err)
	}
	for _, m := range msgs {
		if err := q.Ack(ctx, m.MessageID); err != nil {
			t.Fatalf("Ack() = %v", err)
		}
	}

	// Depth drains as entries are consumed; Len does not, because XACK removes
	// an entry from the pending list but not from the stream.
	depth, err := q.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth() = %v", err)
	}
	length, err := q.Len(ctx)
	if err != nil {
		t.Fatalf("Len() = %v", err)
	}
	if depth != 0 {
		t.Errorf("Depth() = %d after acking everything, want 0", depth)
	}
	if length != 4 {
		t.Errorf("Len() = %d, want 4 — acked entries remain in the stream until trimmed", length)
	}
}

// A Valkey restart without persistence, a flushed database, or an operator
// running FLUSHALL all destroy the stream and its consumer group. Before this
// was handled, every worker wedged permanently on NOGROUP: delivery stopped
// silently while the relay kept enqueueing, because XADD recreates the stream
// but not the group.
//
// Day 5 deletes the Valkey pod on purpose, so this path is load-bearing.
func TestQueueRecoversFromALostConsumerGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := test.NewQueue(t)
	client := test.NewRedisClient(t)

	// Normal operation first, so the group definitely exists.
	if err := q.Enqueue(ctx, uuid.New()); err != nil {
		t.Fatalf("Enqueue() = %v", err)
	}
	msgs, err := q.Claim(ctx, "c1", 10)
	if err != nil {
		t.Fatalf("Claim() = %v", err)
	}
	for _, m := range msgs {
		_ = q.Ack(ctx, m.MessageID)
	}

	// Destroy the stream, exactly as a wiped Valkey would.
	if err := client.Del(ctx, queue.StreamKeyFor(q)).Err(); err != nil {
		t.Fatalf("deleting the stream: %v", err)
	}

	// The relay carries on enqueueing; XADD recreates the stream but not the
	// consumer group.
	want := uuid.New()
	if err := q.Enqueue(ctx, want); err != nil {
		t.Fatalf("Enqueue() after the flush = %v", err)
	}

	// The worker must recover on its own rather than looping on NOGROUP.
	var got []queue.ClaimedMessage
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err = q.Claim(ctx, "c1", 10)
		if err == nil {
			break
		}
		if !errors.Is(err, queue.ErrEmpty) {
			t.Fatalf("Claim() after the flush = %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(got) == 0 {
		t.Fatal("the worker never recovered after the consumer group was destroyed")
	}
	if got[0].EventID != want {
		t.Errorf("recovered event %s, want %s", got[0].EventID, want)
	}
	_ = q.Ack(ctx, got[0].MessageID)
}

// TestEnqueueBatch covers the pipelined producer path the relay uses.
//
// The batching optimisation is only safe if it preserves two properties the
// serial version had: every event arrives exactly once, and each carries its
// OWN trace context rather than sharing one. Both are asserted here.
func TestEnqueueBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("an empty batch is a no-op", func(t *testing.T) {
		q := test.NewQueue(t)
		n, err := q.EnqueueBatch(ctx, nil)
		if err != nil {
			t.Fatalf("EnqueueBatch(nil) = %v", err)
		}
		if n != 0 {
			t.Errorf("enqueued %d, want 0", n)
		}
	})

	t.Run("every event in the batch arrives exactly once", func(t *testing.T) {
		q := test.NewQueue(t)
		const count = 50
		items := make([]queue.EnqueueItem, count)
		want := make(map[uuid.UUID]int, count)
		for i := range items {
			id := uuid.New()
			items[i] = queue.EnqueueItem{EventID: id, Ctx: ctx}
			want[id] = 0
		}

		n, err := q.EnqueueBatch(ctx, items)
		if err != nil {
			t.Fatalf("EnqueueBatch() = %v", err)
		}
		if n != count {
			t.Fatalf("enqueued %d, want %d", n, count)
		}

		// Claim in a loop: one Claim is bounded by count, not by what is there.
		got := 0
		for got < count {
			msgs, err := q.Claim(ctx, "batch-consumer", count)
			if errors.Is(err, queue.ErrEmpty) {
				break
			}
			if err != nil {
				t.Fatalf("Claim() = %v", err)
			}
			for _, m := range msgs {
				if _, ok := want[m.EventID]; !ok {
					t.Errorf("claimed %s, which was never enqueued", m.EventID)
					continue
				}
				want[m.EventID]++
				got++
			}
		}
		if got != count {
			t.Errorf("claimed %d events, want %d", got, count)
		}
		for id, seen := range want {
			if seen != 1 {
				t.Errorf("event %s was delivered %d times, want exactly 1", id, seen)
			}
		}
	})

	t.Run("each item keeps its own trace context", func(t *testing.T) {
		q := test.NewQueue(t)
		// Batching must not collapse many ingests into one trace. Give two
		// items genuinely different spans and assert they stay different.
		tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
		defer func() { _ = tp.Shutdown(ctx) }()
		otel.SetTextMapPropagator(propagation.TraceContext{})
		tracer := tp.Tracer("test")

		ctxA, spanA := tracer.Start(ctx, "ingest-a")
		ctxB, spanB := tracer.Start(ctx, "ingest-b")
		traceA := spanA.SpanContext().TraceID()
		traceB := spanB.SpanContext().TraceID()
		spanA.End()
		spanB.End()
		if traceA == traceB {
			t.Fatal("test setup produced identical trace ids")
		}

		idA, idB := uuid.New(), uuid.New()
		if _, err := q.EnqueueBatch(ctx, []queue.EnqueueItem{
			{EventID: idA, Ctx: ctxA},
			{EventID: idB, Ctx: ctxB},
		}); err != nil {
			t.Fatalf("EnqueueBatch() = %v", err)
		}

		msgs, err := q.Claim(ctx, "trace-consumer", 10)
		if err != nil {
			t.Fatalf("Claim() = %v", err)
		}
		if len(msgs) != 2 {
			t.Fatalf("claimed %d messages, want 2", len(msgs))
		}

		seen := map[uuid.UUID]trace.TraceID{}
		for _, m := range msgs {
			sc := trace.SpanContextFromContext(telemetry.ExtractContext(ctx, m.TraceFields))
			if !sc.IsValid() {
				t.Fatalf("event %s carried no valid trace context", m.EventID)
			}
			seen[m.EventID] = sc.TraceID()
		}
		if seen[idA] != traceA {
			t.Errorf("event A trace = %s, want %s", seen[idA], traceA)
		}
		if seen[idB] != traceB {
			t.Errorf("event B trace = %s, want %s", seen[idB], traceB)
		}
	})
}
