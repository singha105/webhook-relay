package httpapi

import (
	"strings"
	"testing"
)

// A request ID is written into every log line for its request. An ID carrying
// a newline would let a caller forge log entries, and one carrying quotes or
// control characters could corrupt downstream parsing. Go's HTTP client
// refuses to send CRLF in a header, but a raw socket or a non-Go client will,
// so this is tested at the function rather than over the wire.
func TestSanitizeRequestID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"whitespace only becomes empty", "   ", ""},
		{"a uuid passes through", "018f3a4b-7c2d-7e1f-9a3b-2c4d5e6f7a8b", "018f3a4b-7c2d-7e1f-9a3b-2c4d5e6f7a8b"},
		{"a trace-style id passes through", "trace:abc.123_x-9", "trace:abc.123_x-9"},
		{"surrounding whitespace is trimmed", "  abc  ", "abc"},
		{"CRLF injection is stripped", "abc\r\ninjected=true", "abcinjectedtrue"},
		{"newline is stripped", "a\nb", "ab"},
		{"null byte is stripped", "a\x00b", "ab"},
		{"quotes are stripped", `a"b'c`, "abc"},
		// ":" is deliberately allowed so trace-style ids survive intact.
		{"json braces and quotes are stripped", `{"level":"error"}`, "level:error"},
		{"spaces are stripped", "a b c", "abc"},
		{"unicode is stripped", "abc→def", "abcdef"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeRequestID(tt.in)
			if got != tt.want {
				t.Errorf("sanitizeRequestID(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if strings.ContainsAny(got, "\r\n\x00\"'{} ") {
				t.Errorf("result %q still contains a dangerous character", got)
			}
		})
	}
}

func TestSanitizeRequestIDBoundsLength(t *testing.T) {
	// An unbounded caller-supplied value is echoed into every log line for the
	// request, so it has to be capped.
	got := sanitizeRequestID(strings.Repeat("a", maxRequestIDLen*3))
	if len(got) != maxRequestIDLen {
		t.Errorf("len = %d, want %d", len(got), maxRequestIDLen)
	}
}
