package models

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGenerateSigningSecret(t *testing.T) {
	t.Run("has the documented prefix and decodes to 32 bytes", func(t *testing.T) {
		got, err := GenerateSigningSecret()
		if err != nil {
			t.Fatalf("GenerateSigningSecret() error = %v", err)
		}
		if !strings.HasPrefix(got, SigningSecretPrefix) {
			t.Errorf("secret %q missing prefix %q", got, SigningSecretPrefix)
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(got, SigningSecretPrefix))
		if err != nil {
			t.Fatalf("secret body is not raw-url-base64: %v", err)
		}
		if len(raw) != signingSecretEntropyBytes {
			t.Errorf("entropy = %d bytes, want %d", len(raw), signingSecretEntropyBytes)
		}
	})

	t.Run("is URL and header safe", func(t *testing.T) {
		got, err := GenerateSigningSecret()
		if err != nil {
			t.Fatalf("GenerateSigningSecret() error = %v", err)
		}
		if strings.ContainsAny(got, "+/= \t\n") {
			t.Errorf("secret %q contains a character that needs escaping", got)
		}
	})

	// A collision here would mean we had wired up a non-random source. This is
	// a smoke test for that specific mistake, not a statistical test of rand.
	t.Run("does not repeat", func(t *testing.T) {
		const n = 1000
		seen := make(map[string]struct{}, n)
		for i := 0; i < n; i++ {
			s, err := GenerateSigningSecret()
			if err != nil {
				t.Fatalf("GenerateSigningSecret() error = %v", err)
			}
			if _, dup := seen[s]; dup {
				t.Fatalf("duplicate secret generated after %d draws", i)
			}
			seen[s] = struct{}{}
		}
	})
}
