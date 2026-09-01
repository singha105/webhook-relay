package models

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// SigningSecretPrefix marks a string as a webhook-relay signing secret. It
// exists so that a leaked secret is greppable in logs and so that secret
// scanners (including our own gitleaks rules) have a stable pattern to match.
const SigningSecretPrefix = "whsec_"

// signingSecretEntropyBytes is the number of random bytes behind each secret.
// 32 bytes = 256 bits, matching the output width of the SHA-256 HMAC that
// Day 2 will use to sign payloads. Anything less would be the weakest link.
const signingSecretEntropyBytes = 32

// GenerateSigningSecret returns a new endpoint signing secret.
//
// The secret is raw-URL-base64 so it survives being pasted into headers, env
// vars, and YAML without escaping. We use crypto/rand, never math/rand: this
// value is the only thing standing between an attacker and forged webhooks.
func GenerateSigningSecret() (string, error) {
	buf := make([]byte, signingSecretEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate signing secret: %w", err)
	}
	return SigningSecretPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}
