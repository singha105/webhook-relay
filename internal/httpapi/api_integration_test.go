package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/singha105/webhook-relay/internal/httpapi"
	"github.com/singha105/webhook-relay/test"
)

// newTestServer returns a running httptest server backed by real Postgres.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st := test.NewStore(t)
	// Discard logs so a passing run stays readable; a failing test reports
	// through t, not through the log stream.
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := httptest.NewServer(httpapi.NewServer(st, logger, httpapi.ServerConfig{}).Routes())
	t.Cleanup(srv.Close)
	return srv
}

// do issues a request and decodes the JSON response.
func do(t *testing.T, srv *httptest.Server, method, path string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			reader = strings.NewReader(b)
		default:
			raw, err := json.Marshal(b)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			reader = bytes.NewReader(raw)
		}
	}

	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var decoded map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s %s returned non-JSON body %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode, decoded
}

// createEndpoint registers an endpoint and returns its id.
func createEndpoint(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	status, body := do(t, srv, http.MethodPost, "/v1/endpoints",
		map[string]any{"url": "https://example.com/hook", "description": "test"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create endpoint: status = %d, body = %v", status, body)
	}
	return body["id"].(string)
}

func TestHealthProbes(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	t.Run("healthz is up", func(t *testing.T) {
		status, body := do(t, srv, http.MethodGet, "/healthz", nil, nil)
		if status != http.StatusOK || body["status"] != "ok" {
			t.Errorf("healthz = %d %v", status, body)
		}
	})
	t.Run("readyz reports the database is reachable", func(t *testing.T) {
		status, body := do(t, srv, http.MethodGet, "/readyz", nil, nil)
		if status != http.StatusOK || body["status"] != "ready" {
			t.Errorf("readyz = %d %v", status, body)
		}
	})
}

func TestEndpointLifecycle(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	var endpointID string

	t.Run("create returns 201 and the secret exactly once", func(t *testing.T) {
		status, body := do(t, srv, http.MethodPost, "/v1/endpoints",
			map[string]any{"url": "https://example.com/hook", "description": "orders"}, nil)
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %v", status, body)
		}
		secret, ok := body["signing_secret"].(string)
		if !ok || !strings.HasPrefix(secret, "whsec_") {
			t.Fatalf("signing_secret missing or malformed: %v", body["signing_secret"])
		}
		if body["rate_limit_per_sec"] != float64(10) {
			t.Errorf("rate_limit_per_sec = %v, want the default 10", body["rate_limit_per_sec"])
		}
		if body["is_active"] != true {
			t.Errorf("is_active = %v, want true", body["is_active"])
		}
		endpointID = body["id"].(string)
	})

	// The core promise of "returned ONCE".
	t.Run("the secret never appears again", func(t *testing.T) {
		status, body := do(t, srv, http.MethodGet, "/v1/endpoints/"+endpointID, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if _, present := body["signing_secret"]; present {
			t.Error("GET /v1/endpoints/{id} leaked signing_secret")
		}

		_, listBody := do(t, srv, http.MethodGet, "/v1/endpoints", nil, nil)
		raw, _ := json.Marshal(listBody)
		if strings.Contains(string(raw), "whsec_") {
			t.Errorf("GET /v1/endpoints leaked a signing secret: %s", raw)
		}
	})

	t.Run("patch updates only what was sent", func(t *testing.T) {
		status, body := do(t, srv, http.MethodPatch, "/v1/endpoints/"+endpointID,
			map[string]any{"description": "renamed"}, nil)
		if status != http.StatusOK {
			t.Fatalf("status = %d, body = %v", status, body)
		}
		if body["description"] != "renamed" {
			t.Errorf("description = %v", body["description"])
		}
		if body["url"] != "https://example.com/hook" {
			t.Errorf("url was clobbered: %v", body["url"])
		}
	})

	t.Run("list includes the endpoint", func(t *testing.T) {
		status, body := do(t, srv, http.MethodGet, "/v1/endpoints", nil, nil)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		items, ok := body["endpoints"].([]any)
		if !ok || len(items) == 0 {
			t.Fatalf("endpoints = %v, want a non-empty array", body["endpoints"])
		}
	})

	t.Run("delete returns 204 then 404", func(t *testing.T) {
		status, _ := do(t, srv, http.MethodDelete, "/v1/endpoints/"+endpointID, nil, nil)
		if status != http.StatusNoContent {
			t.Fatalf("delete status = %d, want 204", status)
		}
		status, _ = do(t, srv, http.MethodGet, "/v1/endpoints/"+endpointID, nil, nil)
		if status != http.StatusNotFound {
			t.Errorf("get after delete = %d, want 404", status)
		}
	})
}

func TestEndpointValidationErrors(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	tests := []struct {
		name       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{"missing url", map[string]any{}, http.StatusBadRequest, "validation_failed"},
		{"bad scheme", map[string]any{"url": "ftp://example.com"}, http.StatusBadRequest, "validation_failed"},
		{"zero rate limit", map[string]any{"url": "https://a.com", "rate_limit_per_sec": 0}, http.StatusBadRequest, "validation_failed"},
		{"malformed json", `{"url":`, http.StatusBadRequest, "malformed_json"},
		{"unknown field", map[string]any{"url": "https://a.com", "rate_limit": 5}, http.StatusBadRequest, "malformed_json"},
		{"two json objects", `{"url":"https://a.com"}{"url":"https://b.com"}`, http.StatusBadRequest, "malformed_json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := do(t, srv, http.MethodPost, "/v1/endpoints", tt.body, nil)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %v", status, tt.wantStatus, body)
			}
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("response has no error envelope: %v", body)
			}
			if errObj["code"] != tt.wantCode {
				t.Errorf("code = %v, want %q", errObj["code"], tt.wantCode)
			}
			if errObj["request_id"] == "" || errObj["request_id"] == nil {
				t.Error("error body carries no request_id")
			}
		})
	}

	t.Run("a validation error names every broken field at once", func(t *testing.T) {
		status, body := do(t, srv, http.MethodPost, "/v1/endpoints",
			map[string]any{"url": "ftp://x", "rate_limit_per_sec": 0}, nil)
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d", status)
		}
		fields := body["error"].(map[string]any)["fields"].([]any)
		if len(fields) != 2 {
			t.Errorf("fields = %v, want both url and rate_limit_per_sec", fields)
		}
	})
}

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	t.Run("unknown path is 404 with the standard envelope", func(t *testing.T) {
		status, body := do(t, srv, http.MethodGet, "/v1/nope", nil, nil)
		if status != http.StatusNotFound {
			t.Fatalf("status = %d", status)
		}
		if body["error"].(map[string]any)["code"] != "not_found" {
			t.Errorf("code = %v", body["error"])
		}
	})

	t.Run("a malformed uuid is 404, not 400", func(t *testing.T) {
		status, _ := do(t, srv, http.MethodGet, "/v1/endpoints/not-a-uuid", nil, nil)
		if status != http.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("wrong method is 405", func(t *testing.T) {
		status, body := do(t, srv, http.MethodPut, "/v1/endpoints", map[string]any{}, nil)
		if status != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405; body = %v", status, body)
		}
	})
}

func TestRequestIDPropagation(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	t.Run("a generated id is echoed back", func(t *testing.T) {
		resp, err := srv.Client().Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("X-Request-ID") == "" {
			t.Error("no X-Request-ID on the response")
		}
	})

	t.Run("a caller-supplied id is preserved", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
		req.Header.Set("X-Request-ID", "my-trace-42")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("X-Request-ID"); got != "my-trace-42" {
			t.Errorf("X-Request-ID = %q, want my-trace-42", got)
		}
	})

	// Go's own HTTP client refuses to transmit a header value containing CRLF,
	// so the injection case is covered by a unit test on sanitizeRequestID.
	// This asserts on a hostile value the client will actually send.
	t.Run("disallowed characters are stripped", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
		req.Header.Set("X-Request-ID", `abc<script>alert(1)</script>`)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("X-Request-ID"); got != "abcscriptalert1script" {
			t.Errorf("X-Request-ID = %q, want the stripped value", got)
		}
	})
}
