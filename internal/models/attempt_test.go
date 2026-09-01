package models

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateResponseBody(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantLen int
	}{
		{"empty stays empty", "", 0},
		{"short body is untouched", "ok", 2},
		{"exactly at the cap is untouched", strings.Repeat("a", MaxResponseBodyBytes), MaxResponseBodyBytes},
		{"one byte over is clipped", strings.Repeat("a", MaxResponseBodyBytes+1), MaxResponseBodyBytes},
		{"far over is clipped", strings.Repeat("a", MaxResponseBodyBytes*10), MaxResponseBodyBytes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateResponseBody(tt.body)
			if len(got) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(got), tt.wantLen)
			}
			if !strings.HasPrefix(tt.body, got) {
				t.Error("result is not a prefix of the input")
			}
		})
	}

	// The cap is a byte count, so a body of multi-byte runes will land mid-rune.
	// Postgres rejects invalid UTF-8 outright, so this case is a real insert
	// failure, not a cosmetic one.
	t.Run("never splits a multi-byte rune", func(t *testing.T) {
		for _, r := range []string{"é", "→", "😀"} {
			body := strings.Repeat(r, MaxResponseBodyBytes)
			got := TruncateResponseBody(body)
			if !utf8.ValidString(got) {
				t.Errorf("truncating %q produced invalid UTF-8", r)
			}
			if len(got) > MaxResponseBodyBytes {
				t.Errorf("truncating %q produced %d bytes, over the cap", r, len(got))
			}
		}
	})
}
