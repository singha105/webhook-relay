package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/singha105/webhook-relay/internal/models"
	"github.com/singha105/webhook-relay/test"
	"github.com/singha105/webhook-relay/test/sink"
)

// seedEvent registers an endpoint pointing at the pipeline's sink and ingests
// one event, exactly as the API would.
func seedEvent(t *testing.T, p *test.Pipeline) (*models.Endpoint, *models.Event) {
	t.Helper()
	ctx := context.Background()

	secret, err := models.GenerateSigningSecret()
	if err != nil {
		t.Fatalf("GenerateSigningSecret() = %v", err)
	}
	endpoint, err := p.Store.CreateEndpoint(ctx, p.SinkURL, "integration test", secret, 100)
	if err != nil {
		t.Fatalf("CreateEndpoint() = %v", err)
	}

	id, err := models.NewEventID()
	if err != nil {
		t.Fatalf("NewEventID() = %v", err)
	}
	event, _, err := p.Store.CreateEvent(ctx, id, endpoint.ID, "order.created",
		json.RawMessage(`{"order_id":"ord_1","amount":4999}`), nil)
	if err != nil {
		t.Fatalf("CreateEvent() = %v", err)
	}
	return endpoint, event
}

// waitForStatus polls until the event reaches want, or fails with the states it
// actually passed through.
func waitForStatus(t *testing.T, p *test.Pipeline, eventID uuid.UUID, want models.EventStatus, timeout time.Duration) *models.EventWithAttempts {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)

	var last *models.EventWithAttempts
	for time.Now().Before(deadline) {
		got, err := p.Store.GetEventWithAttempts(ctx, eventID)
		if err != nil {
			t.Fatalf("GetEventWithAttempts() = %v", err)
		}
		last = got
		if got.Status == want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}

	if last != nil {
		t.Logf("event %s ended at status %q after %d attempts:", eventID, last.Status, len(last.Attempts))
		for _, a := range last.Attempts {
			code := "none"
			if a.StatusCode != nil {
				code = fmt.Sprint(*a.StatusCode)
			}
			msg := ""
			if a.ErrorMessage != nil {
				msg = *a.ErrorMessage
			}
			t.Logf("  attempt %d: status=%s duration=%dms err=%s", a.AttemptNumber, code, a.DurationMS, msg)
		}
	}
	t.Fatalf("event %s did not reach status %q within %s", eventID, want, timeout)
	return nil
}

func TestDeliverySucceeds(t *testing.T) {
	t.Parallel()
	p := test.NewPipeline(t, test.PipelineConfig{})
	_, event := seedEvent(t, p)

	got := waitForStatus(t, p, event.ID, models.StatusDelivered, 15*time.Second)

	if len(got.Attempts) != 1 {
		t.Errorf("attempts = %d, want exactly 1 for a first-try success", len(got.Attempts))
	}
	if got.Attempts[0].StatusCode == nil || *got.Attempts[0].StatusCode != 200 {
		t.Errorf("attempt status = %v, want 200", got.Attempts[0].StatusCode)
	}
	if got.AttemptCount != 1 {
		t.Errorf("AttemptCount = %d, want 1", got.AttemptCount)
	}

	// The sink is the independent witness: the event really left the process.
	if n := p.Sink.CountFor(event.ID.String()); n != 1 {
		t.Errorf("the sink received %d deliveries, want 1", n)
	}
	if dupes := p.Sink.Duplicates(); len(dupes) != 0 {
		t.Errorf("the sink saw duplicate dispatches: %v", dupes)
	}
}

func TestDeliveryRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	p := test.NewPipeline(t, test.PipelineConfig{MaxAttempts: 5})

	// Deterministic: fail the first two attempts for every event, then accept.
	// A percentage would make this test flaky by construction.
	p.Sink.SetBehavior(sink.Behavior{Status: 200, FailStatus: 503, FailFirstN: 2})

	_, event := seedEvent(t, p)
	got := waitForStatus(t, p, event.ID, models.StatusDelivered, 20*time.Second)

	if len(got.Attempts) != 3 {
		t.Fatalf("attempts = %d, want 3 (two failures then a success)", len(got.Attempts))
	}
	for i, a := range got.Attempts[:2] {
		if a.StatusCode == nil || *a.StatusCode != 503 {
			t.Errorf("attempt %d status = %v, want 503", i+1, a.StatusCode)
		}
	}
	final := got.Attempts[2]
	if final.StatusCode == nil || *final.StatusCode != 200 {
		t.Errorf("final attempt status = %v, want 200", final.StatusCode)
	}

	// Attempt numbers must be contiguous and ordered; the UNIQUE constraint
	// depends on it and so does the audit trail.
	for i, a := range got.Attempts {
		if a.AttemptNumber != i+1 {
			t.Errorf("attempt[%d].AttemptNumber = %d, want %d", i, a.AttemptNumber, i+1)
		}
	}
}

// A 4xx that is not 408 or 429 must not burn the retry budget.
func TestPermanentFailureSkipsRetries(t *testing.T) {
	t.Parallel()
	p := test.NewPipeline(t, test.PipelineConfig{MaxAttempts: 6})
	p.Sink.SetBehavior(sink.Behavior{Status: 404})

	_, event := seedEvent(t, p)
	got := waitForStatus(t, p, event.ID, models.StatusDLQ, 15*time.Second)

	if len(got.Attempts) != 1 {
		t.Errorf("attempts = %d, want exactly 1 — a 404 must not be retried", len(got.Attempts))
	}
	if got.Attempts[0].StatusCode == nil || *got.Attempts[0].StatusCode != 404 {
		t.Errorf("attempt status = %v, want 404", got.Attempts[0].StatusCode)
	}
	// The sink is the proof that we did not keep hammering it.
	if n := p.Sink.CountFor(event.ID.String()); n != 1 {
		t.Errorf("the sink received %d deliveries for a permanent failure, want 1", n)
	}
}

func TestRetriesExhaustIntoTheDLQ(t *testing.T) {
	t.Parallel()
	const maxAttempts = 4
	p := test.NewPipeline(t, test.PipelineConfig{MaxAttempts: maxAttempts})
	p.Sink.SetBehavior(sink.Behavior{Status: 500})

	_, event := seedEvent(t, p)
	got := waitForStatus(t, p, event.ID, models.StatusDLQ, 25*time.Second)

	if len(got.Attempts) != maxAttempts {
		t.Errorf("attempts = %d, want %d", len(got.Attempts), maxAttempts)
	}
	if got.AttemptCount != maxAttempts {
		t.Errorf("AttemptCount = %d, want %d", got.AttemptCount, maxAttempts)
	}
	for _, a := range got.Attempts {
		if a.StatusCode == nil || *a.StatusCode != 500 {
			t.Errorf("attempt %d status = %v, want 500", a.AttemptNumber, a.StatusCode)
		}
	}

	// Attempts are spaced by backoff, so they must not all share a timestamp.
	if len(got.Attempts) >= 2 {
		if got.Attempts[len(got.Attempts)-1].AttemptedAt.Equal(got.Attempts[0].AttemptedAt) {
			t.Error("every attempt has the same timestamp; backoff was not applied")
		}
	}

	// Once dead-lettered, nothing may deliver it again.
	before := p.Sink.CountFor(event.ID.String())
	time.Sleep(time.Second)
	if after := p.Sink.CountFor(event.ID.String()); after != before {
		t.Errorf("a dead-lettered event was delivered again: %d -> %d", before, after)
	}
}

func TestReplayFromTheDLQ(t *testing.T) {
	t.Parallel()
	p := test.NewPipeline(t, test.PipelineConfig{MaxAttempts: 2})
	p.Sink.SetBehavior(sink.Behavior{Status: 500})

	ctx := context.Background()
	_, event := seedEvent(t, p)

	dead := waitForStatus(t, p, event.ID, models.StatusDLQ, 20*time.Second)
	attemptsBefore := len(dead.Attempts)

	// The endpoint recovers.
	p.Sink.SetBehavior(sink.Behavior{Status: 200})

	replayed, err := p.Store.ReplayEvent(ctx, event.ID)
	if err != nil {
		t.Fatalf("ReplayEvent() = %v", err)
	}
	if replayed.Status != models.StatusPending {
		t.Errorf("status after replay = %q, want pending", replayed.Status)
	}
	if replayed.AttemptCount != 0 {
		t.Errorf("AttemptCount after replay = %d, want the budget reset to 0", replayed.AttemptCount)
	}

	got := waitForStatus(t, p, event.ID, models.StatusDelivered, 20*time.Second)

	// The history is kept, not rewritten: an operator investigating the
	// original failure still needs those rows.
	if len(got.Attempts) <= attemptsBefore {
		t.Errorf("attempts = %d, want more than the %d from before the replay",
			len(got.Attempts), attemptsBefore)
	}
	// And the numbering continues rather than colliding with the UNIQUE
	// constraint, which would silently drop the new attempt row.
	for i, a := range got.Attempts {
		if a.AttemptNumber != i+1 {
			t.Errorf("attempt[%d].AttemptNumber = %d, want contiguous numbering", i, a.AttemptNumber)
		}
	}
	last := got.Attempts[len(got.Attempts)-1]
	if last.StatusCode == nil || *last.StatusCode != 200 {
		t.Errorf("final attempt after replay = %v, want 200", last.StatusCode)
	}
}

func TestReplayRejectsAnInFlightEvent(t *testing.T) {
	t.Parallel()
	p := test.NewPipeline(t, test.PipelineConfig{})
	ctx := context.Background()
	_, event := seedEvent(t, p)

	// Wait until it is delivered, then force it into a non-terminal state with
	// a lease far enough out that neither the relay's sweep nor its claim can
	// pick it up and race this assertion.
	waitForStatus(t, p, event.ID, models.StatusDelivered, 15*time.Second)
	if _, err := p.Store.Pool().Exec(ctx,
		`UPDATE events SET status = 'delivering', next_retry_at = now() + interval '1 hour' WHERE id = $1`,
		event.ID); err != nil {
		t.Fatalf("force delivering: %v", err)
	}

	_, err := p.Store.ReplayEvent(ctx, event.ID)
	if err == nil {
		t.Fatal("ReplayEvent() = nil for an in-flight event, want ErrNotReplayable")
	}
	if !errors.Is(err, models.ErrNotReplayable) {
		t.Errorf("ReplayEvent() = %v, want ErrNotReplayable", err)
	}

	t.Run("a missing event is ErrNotFound, not ErrNotReplayable", func(t *testing.T) {
		// These deserve different HTTP answers: 404 for an id that does not
		// exist, 409 for one that does but cannot make this transition.
		_, err := p.Store.ReplayEvent(ctx, uuid.New())
		if !errors.Is(err, models.ErrNotFound) {
			t.Errorf("ReplayEvent() = %v, want ErrNotFound", err)
		}
		if errors.Is(err, models.ErrNotReplayable) {
			t.Error("a missing event was reported as not replayable")
		}
	})
}

// Every delivery must carry a signature the receiver can verify against the
// endpoint's secret, over the exact bytes sent.
func TestDeliveriesAreSigned(t *testing.T) {
	t.Parallel()
	p := test.NewPipeline(t, test.PipelineConfig{})

	endpoint, event := seedEvent(t, p)
	p.EnableSignatureVerification(endpoint.SigningSecret)

	waitForStatus(t, p, event.ID, models.StatusDelivered, 15*time.Second)

	records := p.Sink.Records()
	if len(records) == 0 {
		t.Fatal("the sink recorded no deliveries")
	}
	for _, r := range records {
		if r.SignatureValid == nil {
			t.Fatal("the sink did not verify the signature; the secret was not set in time")
		}
		if !*r.SignatureValid {
			t.Errorf("delivery of %s carried an invalid signature: %s", r.EventID, r.Note)
		}
		if r.Signature == "" {
			t.Error("delivery carried no signature header at all")
		}
	}
}

func TestDeliveryHeaders(t *testing.T) {
	t.Parallel()
	p := test.NewPipeline(t, test.PipelineConfig{MaxAttempts: 4})
	p.Sink.SetBehavior(sink.Behavior{Status: 200, FailStatus: 500, FailFirstN: 1})

	_, event := seedEvent(t, p)
	waitForStatus(t, p, event.ID, models.StatusDelivered, 20*time.Second)

	records := p.Sink.Records()
	if len(records) < 2 {
		t.Fatalf("the sink recorded %d deliveries, want at least 2", len(records))
	}

	// The attempt header has to increment, or a receiver cannot tell a retry
	// from a first delivery.
	for i, r := range records {
		if r.EventID != event.ID.String() {
			t.Errorf("record %d carried event id %q, want %q", i, r.EventID, event.ID)
		}
		if r.Attempt != i+1 {
			t.Errorf("record %d carried attempt %d, want %d", i, r.Attempt, i+1)
		}
	}
}

// An endpoint deactivated while its events are in flight must not be delivered
// to.
func TestInactiveEndpointIsNotDelivered(t *testing.T) {
	t.Parallel()
	p := test.NewPipeline(t, test.PipelineConfig{})
	ctx := context.Background()

	secret, _ := models.GenerateSigningSecret()
	endpoint, err := p.Store.CreateEndpoint(ctx, p.SinkURL, "inactive", secret, 100)
	if err != nil {
		t.Fatalf("CreateEndpoint() = %v", err)
	}
	inactive := false
	if _, err := p.Store.UpdateEndpoint(ctx, endpoint.ID, models.UpdateEndpointRequest{IsActive: &inactive}); err != nil {
		t.Fatalf("UpdateEndpoint() = %v", err)
	}

	id, _ := models.NewEventID()
	event, _, err := p.Store.CreateEvent(ctx, id, endpoint.ID, "t", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("CreateEvent() = %v", err)
	}

	got := waitForStatus(t, p, event.ID, models.StatusDLQ, 15*time.Second)

	if n := p.Sink.CountFor(event.ID.String()); n != 0 {
		t.Errorf("the sink received %d deliveries for an inactive endpoint, want 0", n)
	}
	if len(got.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1 synthetic record explaining why", len(got.Attempts))
	}
	if got.Attempts[0].ErrorMessage == nil {
		t.Error("the synthetic attempt carries no explanation")
	}
	if got.Attempts[0].StatusCode != nil {
		t.Error("the synthetic attempt has a status code; nothing was actually sent")
	}
}

// The endpoint's consecutive_failures counter is what the Day 3 circuit breaker
// will read, so it has to be maintained correctly from the start.
func TestConsecutiveFailureCounter(t *testing.T) {
	t.Parallel()
	p := test.NewPipeline(t, test.PipelineConfig{MaxAttempts: 3})
	p.Sink.SetBehavior(sink.Behavior{Status: 500})

	ctx := context.Background()
	endpoint, event := seedEvent(t, p)
	waitForStatus(t, p, event.ID, models.StatusDLQ, 20*time.Second)

	failed, err := p.Store.GetEndpoint(ctx, endpoint.ID)
	if err != nil {
		t.Fatalf("GetEndpoint() = %v", err)
	}
	if failed.ConsecutiveFailures < 3 {
		t.Errorf("ConsecutiveFailures = %d, want at least 3", failed.ConsecutiveFailures)
	}

	t.Run("a success resets it", func(t *testing.T) {
		p.Sink.SetBehavior(sink.Behavior{Status: 200})

		id, _ := models.NewEventID()
		ev, _, err := p.Store.CreateEvent(ctx, id, endpoint.ID, "t", json.RawMessage(`{}`), nil)
		if err != nil {
			t.Fatalf("CreateEvent() = %v", err)
		}
		waitForStatus(t, p, ev.ID, models.StatusDelivered, 15*time.Second)

		recovered, err := p.Store.GetEndpoint(ctx, endpoint.ID)
		if err != nil {
			t.Fatalf("GetEndpoint() = %v", err)
		}
		if recovered.ConsecutiveFailures != 0 {
			t.Errorf("ConsecutiveFailures = %d after a success, want 0", recovered.ConsecutiveFailures)
		}
	})
}

// Guards the store's DeliveryTarget contract: a worker must see the CURRENT
// endpoint URL and secret, not a copy captured when the message was enqueued.
func TestLoadForDeliveryReadsCurrentRows(t *testing.T) {
	t.Parallel()
	p := test.NewPipeline(t, test.PipelineConfig{})
	ctx := context.Background()

	endpoint, event := seedEvent(t, p)
	waitForStatus(t, p, event.ID, models.StatusDelivered, 15*time.Second)

	newURL := p.SinkURL + "?rotated=1"
	if _, err := p.Store.UpdateEndpoint(ctx, endpoint.ID, models.UpdateEndpointRequest{URL: &newURL}); err != nil {
		t.Fatalf("UpdateEndpoint() = %v", err)
	}

	target, err := p.Store.LoadForDelivery(ctx, event.ID)
	if err != nil {
		t.Fatalf("LoadForDelivery() = %v", err)
	}
	if target.Endpoint.URL != newURL {
		t.Errorf("LoadForDelivery returned URL %q, want the updated %q", target.Endpoint.URL, newURL)
	}
}

func uuidMustParse(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) = %v", s, err)
	}
	return id
}
