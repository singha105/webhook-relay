package models

import (
	"errors"
	"strings"
	"testing"
)

// fieldsOf returns the set of field names that failed validation, so tests can
// assert on *which* rule broke rather than on a formatted message.
func fieldsOf(t *testing.T, err error) map[string]string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v is not a *ValidationError", err)
	}
	out := make(map[string]string, len(ve.Fields))
	for _, f := range ve.Fields {
		out[f.Field] = f.Message
	}
	return out
}

func TestCreateEndpointRequestValidate(t *testing.T) {
	ptr := func(i int) *int { return &i }

	tests := []struct {
		name       string
		req        CreateEndpointRequest
		wantErr    bool
		wantFields []string
	}{
		{
			name: "minimal valid request",
			req:  CreateEndpointRequest{URL: "https://example.com/hook"},
		},
		{
			name: "http is allowed for local testing",
			req:  CreateEndpointRequest{URL: "http://localhost:9000/hook"},
		},
		{
			name: "url with port, path, and query",
			req:  CreateEndpointRequest{URL: "https://example.com:8443/a/b?c=d"},
		},
		{
			name:       "missing url",
			req:        CreateEndpointRequest{},
			wantErr:    true,
			wantFields: []string{"url"},
		},
		{
			name:       "blank url",
			req:        CreateEndpointRequest{URL: "   "},
			wantErr:    true,
			wantFields: []string{"url"},
		},
		{
			name:       "ftp scheme rejected",
			req:        CreateEndpointRequest{URL: "ftp://example.com/hook"},
			wantErr:    true,
			wantFields: []string{"url"},
		},
		{
			name:       "javascript scheme rejected",
			req:        CreateEndpointRequest{URL: "javascript:alert(1)"},
			wantErr:    true,
			wantFields: []string{"url"},
		},
		{
			name:       "scheme-relative url has no scheme",
			req:        CreateEndpointRequest{URL: "//example.com/hook"},
			wantErr:    true,
			wantFields: []string{"url"},
		},
		{
			name:       "url with no host",
			req:        CreateEndpointRequest{URL: "https:///path"},
			wantErr:    true,
			wantFields: []string{"url"},
		},
		{
			name:       "url over the length cap",
			req:        CreateEndpointRequest{URL: "https://example.com/" + strings.Repeat("x", 2048)},
			wantErr:    true,
			wantFields: []string{"url"},
		},
		{
			name: "description over the length cap",
			req: CreateEndpointRequest{
				URL:         "https://example.com/hook",
				Description: strings.Repeat("d", maxDescriptionLen+1),
			},
			wantErr:    true,
			wantFields: []string{"description"},
		},
		{
			name:       "zero rate limit rejected",
			req:        CreateEndpointRequest{URL: "https://example.com/hook", RateLimitPerSec: ptr(0)},
			wantErr:    true,
			wantFields: []string{"rate_limit_per_sec"},
		},
		{
			name:       "negative rate limit rejected",
			req:        CreateEndpointRequest{URL: "https://example.com/hook", RateLimitPerSec: ptr(-1)},
			wantErr:    true,
			wantFields: []string{"rate_limit_per_sec"},
		},
		{
			name:       "rate limit over the cap rejected",
			req:        CreateEndpointRequest{URL: "https://example.com/hook", RateLimitPerSec: ptr(maxRateLimitPerSec + 1)},
			wantErr:    true,
			wantFields: []string{"rate_limit_per_sec"},
		},
		{
			name: "rate limit at the cap accepted",
			req:  CreateEndpointRequest{URL: "https://example.com/hook", RateLimitPerSec: ptr(maxRateLimitPerSec)},
		},
		{
			name:       "every broken rule is reported at once",
			req:        CreateEndpointRequest{URL: "ftp://x", Description: strings.Repeat("d", 999), RateLimitPerSec: ptr(0)},
			wantErr:    true,
			wantFields: []string{"url", "description", "rate_limit_per_sec"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			err := req.Validate()
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
		})
	}
}

// The default only applies when the caller omits the field entirely. An
// explicit 0 must stay an error, which is the whole reason the field is a
// pointer.
func TestCreateEndpointRequestAppliesDefaultRateLimit(t *testing.T) {
	req := CreateEndpointRequest{URL: "https://example.com/hook"}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if req.RateLimitPerSec == nil {
		t.Fatal("RateLimitPerSec was not defaulted")
	}
	if *req.RateLimitPerSec != defaultRateLimitPerS {
		t.Errorf("default = %d, want %d", *req.RateLimitPerSec, defaultRateLimitPerS)
	}
}

func TestUpdateEndpointRequestValidate(t *testing.T) {
	s := func(v string) *string { return &v }
	b := func(v bool) *bool { return &v }
	i := func(v int) *int { return &v }

	tests := []struct {
		name       string
		req        UpdateEndpointRequest
		wantErr    bool
		wantFields []string
	}{
		{name: "url only", req: UpdateEndpointRequest{URL: s("https://example.com/v2")}},
		{name: "deactivate only", req: UpdateEndpointRequest{IsActive: b(false)}},
		{name: "description may be cleared", req: UpdateEndpointRequest{Description: s("")}},
		{name: "rate limit only", req: UpdateEndpointRequest{RateLimitPerSec: i(50)}},
		{
			name:       "empty patch is rejected",
			req:        UpdateEndpointRequest{},
			wantErr:    true,
			wantFields: []string{"body"},
		},
		{
			name:       "bad url in patch",
			req:        UpdateEndpointRequest{URL: s("not-a-url")},
			wantErr:    true,
			wantFields: []string{"url"},
		},
		{
			name:       "bad rate limit in patch",
			req:        UpdateEndpointRequest{RateLimitPerSec: i(0)},
			wantErr:    true,
			wantFields: []string{"rate_limit_per_sec"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Validate() = nil, want error")
				}
				got := fieldsOf(t, err)
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
		})
	}
}
