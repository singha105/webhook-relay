package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/singha105/webhook-relay/internal/models"
	"github.com/singha105/webhook-relay/internal/store"
	"github.com/singha105/webhook-relay/test"
)

func newEventID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := models.NewEventID()
	if err != nil {
		t.Fatalf("NewEventID() = %v", err)
	}
	return id
}

func TestCreateEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := test.NewStore(t)
	e := mustEndpoint(t, ctx, s)

	t.Run("persists with pending status and preserves the payload", func(t *testing.T) {
		payload := json.RawMessage(`{"order_id":"abc","total":19.99,"nested":{"k":[1,2,3]}}`)
		id := newEventID(t)

		ev, created, err := s.CreateEvent(ctx, id, e.ID, "order.created", payload, nil)
		if err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
		if !created {
			t.Error("created = false on a first insert")
		}
		if ev.ID != id {
			t.Errorf("ID = %s, want the UUIDv7 we supplied %s", ev.ID, id)
		}
		if ev.Status != models.StatusPending {
			t.Errorf("Status = %q, want pending", ev.Status)
		}
		if ev.IdempotencyKey != nil {
			t.Errorf("IdempotencyKey = %v, want nil", *ev.IdempotencyKey)
		}

		// JSONB does not preserve byte-for-byte formatting, so compare the
		// decoded structure rather than the raw bytes.
		var want, got map[string]any
		if err := json.Unmarshal(payload, &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(ev.Payload, &got); err != nil {
			t.Fatalf("stored payload is not valid JSON: %v", err)
		}
		if len(got) != len(want) || got["order_id"] != want["order_id"] {
			t.Errorf("payload round-trip mismatch:\n got %v\nwant %v", got, want)
		}
	})

	t.Run("rejects an unknown endpoint with a distinguishable error", func(t *testing.T) {
		_, _, err := s.CreateEvent(ctx, newEventID(t), uuid.New(), "order.created", json.RawMessage(`{}`), nil)
		if !errors.Is(err, store.ErrEndpointNotFound) {
			t.Errorf("error = %v, want store.ErrEndpointNotFound", err)
		}
	})

	t.Run("two events with no idempotency key both persist", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			if _, created, err := s.CreateEvent(ctx, newEventID(t), e.ID, "t", json.RawMessage(`{}`), nil); err != nil || !created {
				t.Fatalf("CreateEvent() = created:%v err:%v", created, err)
			}
		}
	})
}

func TestCreateEventIdempotency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := test.NewStore(t)
	e := mustEndpoint(t, ctx, s)

	t.Run("replaying a key returns the original event", func(t *testing.T) {
		key := "order-123"
		first, created, err := s.CreateEvent(ctx, newEventID(t), e.ID, "order.created", json.RawMessage(`{"v":1}`), &key)
		if err != nil || !created {
			t.Fatalf("first CreateEvent() = created:%v err:%v", created, err)
		}

		// Deliberately different id, type, and payload: a replay must return
		// the ORIGINAL event, not merge the new values in.
		second, created, err := s.CreateEvent(ctx, newEventID(t), e.ID, "totally.different", json.RawMessage(`{"v":999}`), &key)
		if err != nil {
			t.Fatalf("second CreateEvent() = %v", err)
		}
		if created {
			t.Error("created = true on a replayed idempotency key")
		}
		if second.ID != first.ID {
			t.Errorf("replay returned a new event %s, want the original %s", second.ID, first.ID)
		}
		if second.EventType != "order.created" {
			t.Errorf("EventType = %q; the replay overwrote the original", second.EventType)
		}
		var payload map[string]any
		if err := json.Unmarshal(second.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["v"] != float64(1) {
			t.Errorf("payload v = %v, want the original 1", payload["v"])
		}
	})

	t.Run("the same key on a different endpoint is a different event", func(t *testing.T) {
		other := mustEndpoint(t, ctx, s)
		key := "shared-key"

		a, createdA, err := s.CreateEvent(ctx, newEventID(t), e.ID, "t", json.RawMessage(`{}`), &key)
		if err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
		b, createdB, err := s.CreateEvent(ctx, newEventID(t), other.ID, "t", json.RawMessage(`{}`), &key)
		if err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
		if !createdA || !createdB {
			t.Errorf("uniqueness leaked across endpoints: createdA=%v createdB=%v", createdA, createdB)
		}
		if a.ID == b.ID {
			t.Error("two endpoints sharing a key collapsed into one event")
		}
	})
}

// The required race test. Ten goroutines fire the same idempotency key at the
// same instant against a real Postgres. Exactly one row must exist afterwards,
// exactly one caller must see created=true, and all ten must be handed the
// same event.
//
// A SELECT-then-INSERT implementation passes this test only by luck; the
// unique index is what makes it deterministic.
func TestCreateEventIdempotencyRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := test.NewStore(t)
	e := mustEndpoint(t, ctx, s)

	const concurrency = 10
	const key = "race-key"

	// Establish every connection before the race. Without this the pool opens
	// connections lazily, the first goroutine wins outright while the rest are
	// still authenticating, and the test passes implementations that have no
	// business passing it.
	test.WarmPool(t, s.Pool(), concurrency)

	type result struct {
		event   *models.Event
		created bool
		err     error
	}
	results := make([]result, concurrency)

	// A second WaitGroup as a starting gate: every goroutine is fully spun up
	// and blocked on the same signal, so they contend for real rather than
	// trickling in as the scheduler gets to them.
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(concurrency)
	done.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(i int) {
			defer done.Done()
			id := newEventIDNoT()
			k := key
			ready.Done()
			<-start
			ev, created, err := s.CreateEvent(ctx, id, e.ID, "order.created", json.RawMessage(`{"n":1}`), &k)
			results[i] = result{event: ev, created: created, err: err}
		}(i)
	}

	ready.Wait()
	close(start)
	done.Wait()

	var createdCount int
	var winner uuid.UUID
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("goroutine %d: CreateEvent() = %v (a lost race must not surface as an error)", i, r.err)
		}
		if r.created {
			createdCount++
			winner = r.event.ID
		}
	}

	if createdCount != 1 {
		t.Errorf("created=true count = %d, want exactly 1", createdCount)
	}

	// Every caller must have been handed the same event.
	for i, r := range results {
		if r.event.ID != winner {
			t.Errorf("goroutine %d got event %s, want the single winner %s", i, r.event.ID, winner)
		}
	}

	// The database is the source of truth: assert on the row count, not just
	// on what the calls returned.
	var rowCount int
	err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM events WHERE endpoint_id = $1 AND idempotency_key = $2`,
		e.ID, key).Scan(&rowCount)
	if err != nil {
		t.Fatalf("count query = %v", err)
	}
	if rowCount != 1 {
		t.Errorf("events table holds %d rows for key %q, want exactly 1", rowCount, key)
	}
}

// newEventIDNoT is the goroutine-safe variant used inside the race test, where
// calling t.Fatalf from a non-test goroutine would be a data race itself.
func newEventIDNoT() uuid.UUID {
	id, err := models.NewEventID()
	if err != nil {
		// uuid.NewV7 only fails if the system entropy source fails, in which
		// case a random v4 keeps the test meaningful rather than panicking.
		return uuid.New()
	}
	return id
}
