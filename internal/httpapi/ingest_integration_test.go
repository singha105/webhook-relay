package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestIngestEvent(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	endpointID := createEndpoint(t, srv)

	t.Run("returns 202 and a pending event", func(t *testing.T) {
		status, body := do(t, srv, http.MethodPost, "/v1/events", map[string]any{
			"endpoint_id": endpointID,
			"event_type":  "order.created",
			"payload":     map[string]any{"order_id": "abc", "total": 19.99},
		}, nil)

		// 202, not 201: persisted but not delivered.
		if status != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body = %v", status, body)
		}
		if body["status"] != "pending" {
			t.Errorf("status field = %v, want pending", body["status"])
		}
		if body["endpoint_id"] != endpointID {
			t.Errorf("endpoint_id = %v", body["endpoint_id"])
		}
		if body["id"] == nil || body["id"] == "" {
			t.Error("no event id returned")
		}
		payload, ok := body["payload"].(map[string]any)
		if !ok || payload["order_id"] != "abc" {
			t.Errorf("payload did not round-trip: %v", body["payload"])
		}
	})

	t.Run("the event is readable with an empty attempt history", func(t *testing.T) {
		_, created := do(t, srv, http.MethodPost, "/v1/events", map[string]any{
			"endpoint_id": endpointID,
			"event_type":  "order.shipped",
			"payload":     map[string]any{"n": 1},
		}, nil)
		eventID := created["id"].(string)

		status, body := do(t, srv, http.MethodGet, "/v1/events/"+eventID, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		attempts, ok := body["attempts"].([]any)
		if !ok {
			t.Fatalf("attempts = %v, want [] (nothing is delivered on day 1)", body["attempts"])
		}
		if len(attempts) != 0 {
			t.Errorf("len(attempts) = %d, want 0", len(attempts))
		}
	})

	t.Run("an unknown endpoint is 422, not 404", func(t *testing.T) {
		status, body := do(t, srv, http.MethodPost, "/v1/events", map[string]any{
			"endpoint_id": "018f3a4b-7c2d-7e1f-9a3b-2c4d5e6f7a8b",
			"event_type":  "order.created",
			"payload":     map[string]any{},
		}, nil)
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", status, body)
		}
		if body["error"].(map[string]any)["code"] != "endpoint_not_found" {
			t.Errorf("code = %v", body["error"])
		}
	})

	t.Run("validation failures", func(t *testing.T) {
		tests := []struct {
			name string
			body map[string]any
		}{
			{"missing endpoint_id", map[string]any{"event_type": "t", "payload": map[string]any{}}},
			{"malformed endpoint_id", map[string]any{"endpoint_id": "nope", "event_type": "t", "payload": map[string]any{}}},
			{"missing event_type", map[string]any{"endpoint_id": endpointID, "payload": map[string]any{}}},
			{"missing payload", map[string]any{"endpoint_id": endpointID, "event_type": "t"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				status, body := do(t, srv, http.MethodPost, "/v1/events", tt.body, nil)
				if status != http.StatusBadRequest {
					t.Errorf("status = %d, want 400; body = %v", status, body)
				}
			})
		}
	})

	t.Run("an unknown event id is 404", func(t *testing.T) {
		status, _ := do(t, srv, http.MethodGet, "/v1/events/018f3a4b-7c2d-7e1f-9a3b-2c4d5e6f7a8b", nil, nil)
		if status != http.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("an oversized body is rejected before decoding", func(t *testing.T) {
		huge := `{"endpoint_id":"` + endpointID + `","event_type":"big","payload":{"blob":"` +
			strings.Repeat("x", 2<<20) + `"}}`
		status, _ := do(t, srv, http.MethodPost, "/v1/events", huge, nil)
		if status != http.StatusRequestEntityTooLarge && status != http.StatusBadRequest {
			t.Errorf("status = %d, want 413", status)
		}
	})
}

func TestIngestIdempotency(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	endpointID := createEndpoint(t, srv)

	body := map[string]any{
		"endpoint_id": endpointID,
		"event_type":  "order.created",
		"payload":     map[string]any{"order_id": "idem-1"},
	}
	headers := map[string]string{"Idempotency-Key": "order-idem-1"}

	t.Run("first request is 202", func(t *testing.T) {
		status, resp := do(t, srv, http.MethodPost, "/v1/events", body, headers)
		if status != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body = %v", status, resp)
		}
	})

	var firstID string
	t.Run("replay is 200 with the original event", func(t *testing.T) {
		_, first := do(t, srv, http.MethodPost, "/v1/events", body, headers)
		firstID = first["id"].(string)

		// Different payload on the replay: the original must win.
		replay := map[string]any{
			"endpoint_id": endpointID,
			"event_type":  "something.else",
			"payload":     map[string]any{"order_id": "DIFFERENT"},
		}
		status, resp := do(t, srv, http.MethodPost, "/v1/events", replay, headers)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 on replay; body = %v", status, resp)
		}
		if resp["id"] != firstID {
			t.Errorf("replay returned event %v, want the original %v", resp["id"], firstID)
		}
		if resp["event_type"] != "order.created" {
			t.Errorf("event_type = %v; the replay overwrote the original", resp["event_type"])
		}
	})

	t.Run("no key means no deduplication", func(t *testing.T) {
		_, a := do(t, srv, http.MethodPost, "/v1/events", body, nil)
		_, b := do(t, srv, http.MethodPost, "/v1/events", body, nil)
		if a["id"] == b["id"] {
			t.Error("two un-keyed requests collapsed into one event")
		}
	})

	t.Run("the same key on a different endpoint is a different event", func(t *testing.T) {
		other := createEndpoint(t, srv)
		otherBody := map[string]any{
			"endpoint_id": other,
			"event_type":  "order.created",
			"payload":     map[string]any{},
		}
		status, resp := do(t, srv, http.MethodPost, "/v1/events", otherBody, headers)
		if status != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 — the key leaked across endpoints", status)
		}
		if resp["id"] == firstID {
			t.Error("the same event was returned for a different endpoint")
		}
	})
}

// The Day 1 acceptance criterion, exercised over real HTTP rather than at the
// store layer: ten concurrent identical POSTs must produce exactly one event.
func TestIngestIdempotencyRaceOverHTTP(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	endpointID := createEndpoint(t, srv)

	const concurrency = 10
	payload, err := json.Marshal(map[string]any{
		"endpoint_id": endpointID,
		"event_type":  "order.created",
		"payload":     map[string]any{"order_id": "race"},
	})
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		status int
		id     string
		err    error
	}
	outcomes := make([]outcome, concurrency)

	// Force the connection pool to establish every connection before the
	// starting gate opens; otherwise the goroutines serialize behind TCP
	// setup and never actually contend.
	warmHTTPClient(t, srv, concurrency)

	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(concurrency)
	done.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(i int) {
			defer done.Done()
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/events", bytes.NewReader(payload))
			if err != nil {
				outcomes[i] = outcome{err: err}
				ready.Done()
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "concurrent-key")

			ready.Done()
			<-start

			resp, err := srv.Client().Do(req)
			if err != nil {
				outcomes[i] = outcome{err: err}
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			var decoded map[string]any
			_ = json.Unmarshal(raw, &decoded)
			id, _ := decoded["id"].(string)
			outcomes[i] = outcome{status: resp.StatusCode, id: id}
		}(i)
	}

	ready.Wait()
	close(start)
	done.Wait()

	var accepted int
	ids := map[string]int{}
	for i, o := range outcomes {
		if o.err != nil {
			t.Fatalf("goroutine %d: %v", i, o.err)
		}
		switch o.status {
		case http.StatusAccepted:
			accepted++
		case http.StatusOK:
		default:
			t.Fatalf("goroutine %d: status = %d, want 202 or 200 — a lost race must not surface as an error", i, o.status)
		}
		ids[o.id]++
	}

	if accepted != 1 {
		t.Errorf("202 count = %d, want exactly 1 (the rest must be 200)", accepted)
	}
	if len(ids) != 1 {
		t.Errorf("callers saw %d distinct event ids, want 1: %v", len(ids), ids)
	}

	// The API is the source of truth for this assertion: fetch the one event
	// and confirm it is real.
	for id := range ids {
		status, _ := do(t, srv, http.MethodGet, "/v1/events/"+id, nil, nil)
		if status != http.StatusOK {
			t.Errorf("the winning event %s is not readable: status %d", id, status)
		}
	}
}

// warmHTTPClient opens n keep-alive connections so the race test contends on
// the server rather than on TCP handshakes.
func warmHTTPClient(t *testing.T, srv *httptest.Server, n int) {
	t.Helper()

	srv.Client().Transport.(*http.Transport).MaxIdleConnsPerHost = n

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			resp, err := srv.Client().Get(srv.URL + "/healthz")
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
}
