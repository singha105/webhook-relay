package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCreateEventRequestValidate(t *testing.T) {
	const validID = "018f3a4b-7c2d-7e1f-9a3b-2c4d5e6f7a8b"

	tests := []struct {
		name       string
		req        CreateEventRequest
		wantErr    bool
		wantFields []string
	}{
		{
			name: "valid request",
			req:  CreateEventRequest{EndpointID: validID, EventType: "order.created", Payload: json.RawMessage(`{"id":1}`)},
		},
		{
			name: "payload may be a JSON array",
			req:  CreateEventRequest{EndpointID: validID, EventType: "batch", Payload: json.RawMessage(`[1,2,3]`)},
		},
		{
			name: "payload may be an empty object",
			req:  CreateEventRequest{EndpointID: validID, EventType: "ping", Payload: json.RawMessage(`{}`)},
		},
		{
			name:       "missing endpoint id",
			req:        CreateEventRequest{EventType: "order.created", Payload: json.RawMessage(`{}`)},
			wantErr:    true,
			wantFields: []string{"endpoint_id"},
		},
		{
			name:       "malformed endpoint id",
			req:        CreateEventRequest{EndpointID: "nope", EventType: "order.created", Payload: json.RawMessage(`{}`)},
			wantErr:    true,
			wantFields: []string{"endpoint_id"},
		},
		{
			name:       "missing event type",
			req:        CreateEventRequest{EndpointID: validID, Payload: json.RawMessage(`{}`)},
			wantErr:    true,
			wantFields: []string{"event_type"},
		},
		{
			name:       "event type over the cap",
			req:        CreateEventRequest{EndpointID: validID, EventType: strings.Repeat("t", maxEventTypeLen+1), Payload: json.RawMessage(`{}`)},
			wantErr:    true,
			wantFields: []string{"event_type"},
		},
		{
			name:       "missing payload",
			req:        CreateEventRequest{EndpointID: validID, EventType: "order.created"},
			wantErr:    true,
			wantFields: []string{"payload"},
		},
		{
			name:       "malformed payload",
			req:        CreateEventRequest{EndpointID: validID, EventType: "order.created", Payload: json.RawMessage(`{"a":}`)},
			wantErr:    true,
			wantFields: []string{"payload"},
		},
		{
			name:       "payload over the size cap",
			req:        CreateEventRequest{EndpointID: validID, EventType: "big", Payload: json.RawMessage(`"` + strings.Repeat("x", maxPayloadBytes) + `"`)},
			wantErr:    true,
			wantFields: []string{"payload"},
		},
		{
			name:       "all three fields broken",
			req:        CreateEventRequest{EndpointID: "bad", Payload: json.RawMessage(`{`)},
			wantErr:    true,
			wantFields: []string{"endpoint_id", "event_type", "payload"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			id, err := req.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Validate() = nil, want error")
				}
				got := fieldsOf(t, err)
				if len(got) != len(tt.wantFields) {
					t.Errorf("failed fields = %v, want exactly %v", got, tt.wantFields)
				}
				for _, f := range tt.wantFields {
					if _, ok := got[f]; !ok {
						t.Errorf("expected field %q to fail; got %v", f, got)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if id.String() != tt.req.EndpointID {
				t.Errorf("parsed endpoint id = %s, want %s", id, tt.req.EndpointID)
			}
		})
	}
}

func TestValidateIdempotencyKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{name: "absent header is allowed", key: ""},
		{name: "ordinary key", key: "order-123"},
		{name: "key at the cap", key: strings.Repeat("k", maxIdempotencyKeyLen)},
		{name: "key over the cap", key: strings.Repeat("k", maxIdempotencyKeyLen+1), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateIdempotencyKey(tt.key)
			if tt.wantErr && err == nil {
				t.Error("ValidateIdempotencyKey() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ValidateIdempotencyKey() = %v, want nil", err)
			}
		})
	}
}

func TestEventStatusValid(t *testing.T) {
	valid := []EventStatus{StatusPending, StatusDelivering, StatusDelivered, StatusFailed, StatusDLQ}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("%q.Valid() = false, want true", s)
		}
	}
	for _, s := range []EventStatus{"", "PENDING", "done", "retrying"} {
		if s.Valid() {
			t.Errorf("%q.Valid() = true, want false", s)
		}
	}
}

// UUIDv7 encodes a millisecond timestamp in its leading 48 bits. Both
// properties below are the reason we chose v7 over v4, so both are worth
// pinning down in a test.
func TestNewEventIDIsTimeOrdered(t *testing.T) {
	t.Run("version is 7", func(t *testing.T) {
		id, err := NewEventID()
		if err != nil {
			t.Fatalf("NewEventID() error = %v", err)
		}
		if got := id.Version().String(); got != "VERSION_7" {
			t.Errorf("version = %s, want VERSION_7", got)
		}
	})

	t.Run("ids sort in creation order", func(t *testing.T) {
		const n = 50
		ids := make([]string, 0, n)
		for i := 0; i < n; i++ {
			id, err := NewEventID()
			if err != nil {
				t.Fatalf("NewEventID() error = %v", err)
			}
			ids = append(ids, id.String())
			time.Sleep(1100 * time.Microsecond) // cross a millisecond boundary
		}
		for i := 1; i < len(ids); i++ {
			if ids[i-1] >= ids[i] {
				t.Fatalf("ids not ascending at %d: %s >= %s", i, ids[i-1], ids[i])
			}
		}
	})
}
