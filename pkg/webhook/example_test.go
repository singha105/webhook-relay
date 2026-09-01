package webhook_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/singha105/webhook-relay/pkg/webhook"
)

// ExampleVerify is the handler a receiver writes. It is the only thing most
// integrators need from this package.
func ExampleVerify() {
	const signingSecret = "whsec_from_your_endpoint_registration"

	handler := func(w http.ResponseWriter, r *http.Request) {
		// Read the body once, with a bound. The signature covers these exact
		// bytes.
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}

		// Verify BEFORE unmarshalling. Never unmarshal, re-marshal, and verify
		// the result: JSON round-tripping reorders keys and changes whitespace,
		// which changes the digest.
		if err := webhook.Verify(webhook.VerifyParams{
			Secret:    signingSecret,
			Header:    r.Header.Get(webhook.HeaderSignature),
			Body:      body,
			Tolerance: webhook.DefaultTolerance,
		}); err != nil {
			// 401 and nothing else. Do not echo the reason back — it tells an
			// attacker whether they got the secret wrong or the timestamp.
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}

		// Delivery is at-least-once: this id can legitimately arrive more than
		// once, so deduplicate on it before doing anything with side effects.
		eventID := r.Header.Get(webhook.HeaderID)
		attempt := r.Header.Get(webhook.HeaderAttempt)
		fmt.Printf("event %s (attempt %s) verified\n", eventID, attempt)

		// Respond 2xx quickly. Anything slow belongs on your own queue —
		// webhook-relay times out at 10 seconds and will retry.
		w.WriteHeader(http.StatusOK)
	}

	_ = handler
	// Output:
}

// ExampleSign shows how the sender builds the header, which is useful when
// reproducing a mismatch by hand.
//
// The digest below is cross-checked against openssl and python hmac, so this
// example doubles as a conformance vector for a receiver written in another
// language:
//
//	printf '1700000000.{"event":"order.created"}' | \
//	    openssl dgst -sha256 -hmac "whsec_example" -r
func ExampleSign() {
	secret := "whsec_example"
	body := []byte(`{"event":"order.created"}`)
	ts := time.Unix(1700000000, 0)

	header := webhook.Sign(secret, ts, body)
	fmt.Println(header)

	fmt.Printf("signed payload: %s\n", webhook.SignedPayload(ts, body))

	// Output:
	// t=1700000000,v1=51f4ce9aa18a528ba8ad85b82ec1ee495fe64993669a8ef933b1577754dfdbe9
	// signed payload: 1700000000.{"event":"order.created"}
}
