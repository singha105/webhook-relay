package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/singha105/webhook-relay/internal/models"
	"github.com/singha105/webhook-relay/test"
)

func TestRecordAndListAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := test.NewStore(t)
	e := mustEndpoint(t, ctx, s)

	ev, _, err := s.CreateEvent(ctx, newEventID(t), e.ID, "order.created", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("CreateEvent() = %v", err)
	}

	t.Run("attempts come back in attempt_number order", func(t *testing.T) {
		codes := []int{500, 502, 200}
		for i, c := range codes {
			code := c
			if err := s.RecordAttempt(ctx, models.DeliveryAttempt{
				EventID: ev.ID, AttemptNumber: i + 1, StatusCode: &code,
				ResponseBody: "body", DurationMS: 10 * (i + 1),
			}); err != nil {
				t.Fatalf("RecordAttempt(%d) = %v", i+1, err)
			}
		}
		got, err := s.ListAttempts(ctx, ev.ID)
		if err != nil {
			t.Fatalf("ListAttempts() = %v", err)
		}
		if len(got) != len(codes) {
			t.Fatalf("len = %d, want %d", len(got), len(codes))
		}
		for i, a := range got {
			if a.AttemptNumber != i+1 {
				t.Errorf("attempt[%d].AttemptNumber = %d, want %d", i, a.AttemptNumber, i+1)
			}
			if a.StatusCode == nil || *a.StatusCode != codes[i] {
				t.Errorf("attempt[%d].StatusCode = %v, want %d", i, a.StatusCode, codes[i])
			}
			if a.AttemptedAt.IsZero() {
				t.Errorf("attempt[%d].AttemptedAt was not defaulted", i)
			}
		}
	})

	// A transport failure has no HTTP response at all. Day 3's retry policy
	// treats that differently from a real 5xx, so the NULL has to survive.
	t.Run("a transport failure stores a null status code", func(t *testing.T) {
		ev2, _, err := s.CreateEvent(ctx, newEventID(t), e.ID, "t", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
		msg := "dial tcp 10.0.0.1:443: i/o timeout"
		if err := s.RecordAttempt(ctx, models.DeliveryAttempt{
			EventID: ev2.ID, AttemptNumber: 1, StatusCode: nil,
			ErrorMessage: &msg, DurationMS: 30000,
		}); err != nil {
			t.Fatalf("RecordAttempt() = %v", err)
		}
		got, err := s.ListAttempts(ctx, ev2.ID)
		if err != nil {
			t.Fatalf("ListAttempts() = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		if got[0].StatusCode != nil {
			t.Errorf("StatusCode = %d, want nil", *got[0].StatusCode)
		}
		if got[0].ErrorMessage == nil || *got[0].ErrorMessage != msg {
			t.Errorf("ErrorMessage = %v, want %q", got[0].ErrorMessage, msg)
		}
	})

	// A worker that crashes after writing attempt 3 and retries must not
	// create a second attempt 3.
	t.Run("re-recording the same attempt number is a no-op", func(t *testing.T) {
		ev3, _, err := s.CreateEvent(ctx, newEventID(t), e.ID, "t", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
		code := 503
		attempt := models.DeliveryAttempt{
			EventID: ev3.ID, AttemptNumber: 1, StatusCode: &code, DurationMS: 5,
		}
		for i := 0; i < 3; i++ {
			if err := s.RecordAttempt(ctx, attempt); err != nil {
				t.Fatalf("RecordAttempt() call %d = %v", i+1, err)
			}
		}
		got, err := s.ListAttempts(ctx, ev3.ID)
		if err != nil {
			t.Fatalf("ListAttempts() = %v", err)
		}
		if len(got) != 1 {
			t.Errorf("len = %d, want 1 — duplicate attempt numbers were written", len(got))
		}
	})

	// The Go helper truncates before the driver sees the value; the CHECK
	// constraint is only a backstop. If truncation regressed, this insert
	// would fail rather than silently storing an oversized row.
	t.Run("an oversized response body is truncated, not rejected", func(t *testing.T) {
		ev4, _, err := s.CreateEvent(ctx, newEventID(t), e.ID, "t", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
		code := 500
		huge := strings.Repeat("x", models.MaxResponseBodyBytes*4)
		if err := s.RecordAttempt(ctx, models.DeliveryAttempt{
			EventID: ev4.ID, AttemptNumber: 1, StatusCode: &code,
			ResponseBody: huge, DurationMS: 1,
		}); err != nil {
			t.Fatalf("RecordAttempt() = %v", err)
		}
		got, err := s.ListAttempts(ctx, ev4.ID)
		if err != nil {
			t.Fatalf("ListAttempts() = %v", err)
		}
		if len(got[0].ResponseBody) != models.MaxResponseBodyBytes {
			t.Errorf("stored body = %d bytes, want %d", len(got[0].ResponseBody), models.MaxResponseBodyBytes)
		}
	})

	t.Run("an event with no attempts returns an empty slice, not nil", func(t *testing.T) {
		ev5, _, err := s.CreateEvent(ctx, newEventID(t), e.ID, "t", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
		got, err := s.ListAttempts(ctx, ev5.ID)
		if err != nil {
			t.Fatalf("ListAttempts() = %v", err)
		}
		if got == nil {
			t.Error("nil slice would encode as JSON null instead of []")
		}
		if len(got) != 0 {
			t.Errorf("len = %d, want 0", len(got))
		}
	})
}

func TestGetEventWithAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := test.NewStore(t)
	e := mustEndpoint(t, ctx, s)

	ev, _, err := s.CreateEvent(ctx, newEventID(t), e.ID, "order.created", json.RawMessage(`{"x":1}`), nil)
	if err != nil {
		t.Fatalf("CreateEvent() = %v", err)
	}
	code := 500
	if err := s.RecordAttempt(ctx, models.DeliveryAttempt{
		EventID: ev.ID, AttemptNumber: 1, StatusCode: &code, DurationMS: 42,
	}); err != nil {
		t.Fatalf("RecordAttempt() = %v", err)
	}

	got, err := s.GetEventWithAttempts(ctx, ev.ID)
	if err != nil {
		t.Fatalf("GetEventWithAttempts() = %v", err)
	}
	if got.ID != ev.ID {
		t.Errorf("ID = %s, want %s", got.ID, ev.ID)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].DurationMS != 42 {
		t.Errorf("attempts = %+v, want one attempt with duration 42", got.Attempts)
	}
}

// Exercises the partial index events (endpoint_id, created_at) WHERE
// status='pending' — both that it returns oldest-first and that a delivered
// event drops out of the result set.
func TestListPendingEventsForEndpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := test.NewStore(t)
	e := mustEndpoint(t, ctx, s)

	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		ev, _, err := s.CreateEvent(ctx, newEventID(t), e.ID, "t", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
		ids = append(ids, ev.ID.String())
	}

	got, err := s.ListPendingEventsForEndpoint(ctx, e.ID, 10)
	if err != nil {
		t.Fatalf("ListPendingEventsForEndpoint() = %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].CreatedAt.After(got[i].CreatedAt) {
			t.Error("results are not oldest-first")
		}
	}

	t.Run("limit is honoured", func(t *testing.T) {
		limited, err := s.ListPendingEventsForEndpoint(ctx, e.ID, 2)
		if err != nil {
			t.Fatalf("ListPendingEventsForEndpoint() = %v", err)
		}
		if len(limited) != 2 {
			t.Errorf("len = %d, want 2", len(limited))
		}
	})

	t.Run("a delivered event leaves the pending set", func(t *testing.T) {
		if err := s.UpdateEventStatus(ctx, uuidMust(t, ids[0]), models.StatusDelivered); err != nil {
			t.Fatalf("UpdateEventStatus() = %v", err)
		}
		after, err := s.ListPendingEventsForEndpoint(ctx, e.ID, 10)
		if err != nil {
			t.Fatalf("ListPendingEventsForEndpoint() = %v", err)
		}
		if len(after) != 4 {
			t.Errorf("len = %d, want 4 after one delivery", len(after))
		}
	})

	t.Run("an invalid status is rejected before it reaches the driver", func(t *testing.T) {
		err := s.UpdateEventStatus(ctx, uuidMust(t, ids[1]), models.EventStatus("bogus"))
		if err == nil {
			t.Error("UpdateEventStatus() = nil, want error for an invalid status")
		}
	})
}
