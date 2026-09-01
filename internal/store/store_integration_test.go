package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/singha105/webhook-relay/internal/models"
	"github.com/singha105/webhook-relay/internal/store"
	"github.com/singha105/webhook-relay/test"
)

// mustEndpoint creates an endpoint and fails the test if it cannot.
func mustEndpoint(t *testing.T, ctx context.Context, s *store.Store) *models.Endpoint {
	t.Helper()
	secret, err := models.GenerateSigningSecret()
	if err != nil {
		t.Fatalf("GenerateSigningSecret() = %v", err)
	}
	e, err := s.CreateEndpoint(ctx, "https://example.com/hook", "test endpoint", secret, 10)
	if err != nil {
		t.Fatalf("CreateEndpoint() = %v", err)
	}
	return e
}

func TestEndpointCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := test.NewStore(t)

	t.Run("create applies defaults and returns generated columns", func(t *testing.T) {
		e := mustEndpoint(t, ctx, s)
		if e.ID == uuid.Nil {
			t.Error("ID was not generated")
		}
		if !e.IsActive {
			t.Error("IsActive should default to true")
		}
		if e.ConsecutiveFailures != 0 {
			t.Errorf("ConsecutiveFailures = %d, want 0", e.ConsecutiveFailures)
		}
		if e.CreatedAt.IsZero() || e.UpdatedAt.IsZero() {
			t.Error("timestamps were not populated")
		}
		if !strings.HasPrefix(e.SigningSecret, models.SigningSecretPrefix) {
			t.Errorf("signing secret was not round-tripped: %q", e.SigningSecret)
		}
	})

	t.Run("get returns the stored row", func(t *testing.T) {
		created := mustEndpoint(t, ctx, s)
		got, err := s.GetEndpoint(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetEndpoint() = %v", err)
		}
		if got.ID != created.ID || got.URL != created.URL {
			t.Errorf("GetEndpoint() = %+v, want %+v", got, created)
		}
	})

	t.Run("get of an unknown id is ErrNotFound", func(t *testing.T) {
		_, err := s.GetEndpoint(ctx, uuid.New())
		if !errors.Is(err, models.ErrNotFound) {
			t.Errorf("error = %v, want models.ErrNotFound", err)
		}
	})

	t.Run("patch leaves omitted fields alone", func(t *testing.T) {
		created := mustEndpoint(t, ctx, s)
		newDesc := "updated description"

		got, err := s.UpdateEndpoint(ctx, created.ID, models.UpdateEndpointRequest{Description: &newDesc})
		if err != nil {
			t.Fatalf("UpdateEndpoint() = %v", err)
		}
		if got.Description != newDesc {
			t.Errorf("Description = %q, want %q", got.Description, newDesc)
		}
		// The whole point of COALESCE: everything else survives.
		if got.URL != created.URL {
			t.Errorf("URL was clobbered: %q -> %q", created.URL, got.URL)
		}
		if got.IsActive != created.IsActive {
			t.Error("IsActive was clobbered")
		}
		if got.RateLimitPerSec != created.RateLimitPerSec {
			t.Error("RateLimitPerSec was clobbered")
		}
		if !got.UpdatedAt.After(created.UpdatedAt) {
			t.Error("updated_at trigger did not fire")
		}
	})

	t.Run("patch can deactivate", func(t *testing.T) {
		created := mustEndpoint(t, ctx, s)
		inactive := false
		got, err := s.UpdateEndpoint(ctx, created.ID, models.UpdateEndpointRequest{IsActive: &inactive})
		if err != nil {
			t.Fatalf("UpdateEndpoint() = %v", err)
		}
		if got.IsActive {
			t.Error("IsActive = true, want false")
		}
	})

	t.Run("patch of an unknown id is ErrNotFound", func(t *testing.T) {
		desc := "x"
		_, err := s.UpdateEndpoint(ctx, uuid.New(), models.UpdateEndpointRequest{Description: &desc})
		if !errors.Is(err, models.ErrNotFound) {
			t.Errorf("error = %v, want models.ErrNotFound", err)
		}
	})

	t.Run("delete removes the row", func(t *testing.T) {
		created := mustEndpoint(t, ctx, s)
		if err := s.DeleteEndpoint(ctx, created.ID); err != nil {
			t.Fatalf("DeleteEndpoint() = %v", err)
		}
		if _, err := s.GetEndpoint(ctx, created.ID); !errors.Is(err, models.ErrNotFound) {
			t.Errorf("endpoint still readable after delete: %v", err)
		}
	})

	t.Run("delete of an unknown id is ErrNotFound", func(t *testing.T) {
		if err := s.DeleteEndpoint(ctx, uuid.New()); !errors.Is(err, models.ErrNotFound) {
			t.Errorf("error = %v, want models.ErrNotFound", err)
		}
	})

	t.Run("list returns newest first and never nil", func(t *testing.T) {
		s2 := test.NewStore(t) // isolated schema so counts are deterministic
		empty, err := s2.ListEndpoints(ctx, 10, 0)
		if err != nil {
			t.Fatalf("ListEndpoints() = %v", err)
		}
		if empty == nil {
			t.Error("empty list is nil; JSON would encode as null instead of []")
		}
		for i := 0; i < 3; i++ {
			mustEndpoint(t, ctx, s2)
		}
		got, err := s2.ListEndpoints(ctx, 10, 0)
		if err != nil {
			t.Fatalf("ListEndpoints() = %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].CreatedAt.Before(got[i].CreatedAt) {
				t.Error("results are not newest-first")
			}
		}
	})
}

func TestEndpointDeleteCascades(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := test.NewStore(t)

	e := mustEndpoint(t, ctx, s)
	id, err := models.NewEventID()
	if err != nil {
		t.Fatalf("NewEventID() = %v", err)
	}
	ev, _, err := s.CreateEvent(ctx, id, e.ID, "order.created", json.RawMessage(`{"a":1}`), nil)
	if err != nil {
		t.Fatalf("CreateEvent() = %v", err)
	}
	code := 500
	if err := s.RecordAttempt(ctx, models.DeliveryAttempt{
		EventID: ev.ID, AttemptNumber: 1, StatusCode: &code, DurationMS: 12,
	}); err != nil {
		t.Fatalf("RecordAttempt() = %v", err)
	}

	if err := s.DeleteEndpoint(ctx, e.ID); err != nil {
		t.Fatalf("DeleteEndpoint() = %v", err)
	}

	// The cascade has to reach two levels down, endpoint -> event -> attempt.
	if _, err := s.GetEvent(ctx, ev.ID); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("event survived endpoint delete: %v", err)
	}
	attempts, err := s.ListAttempts(ctx, ev.ID)
	if err != nil {
		t.Fatalf("ListAttempts() = %v", err)
	}
	if len(attempts) != 0 {
		t.Errorf("%d attempts survived the cascade", len(attempts))
	}
}

// uuidMust parses a UUID string in a test context.
func uuidMust(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) = %v", s, err)
	}
	return id
}
