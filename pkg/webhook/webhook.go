// Package webhook verifies webhook-relay signatures.
//
// This package is meant to be copied or imported by the *receiver* of a
// webhook, not just by this service. It therefore depends on nothing outside
// the standard library, and its API is deliberately small: one function you
// call from your HTTP handler.
//
// # Verifying a webhook
//
//	func handler(w http.ResponseWriter, r *http.Request) {
//	    body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
//	    if err != nil {
//	        http.Error(w, "read failed", http.StatusBadRequest)
//	        return
//	    }
//
//	    // The signature covers the EXACT bytes that were sent. Verify before
//	    // unmarshalling, and never re-marshal and verify the result — JSON
//	    // round-tripping reorders keys and changes whitespace, which changes
//	    // the digest.
//	    err = webhook.Verify(webhook.VerifyParams{
//	        Secret:    os.Getenv("WEBHOOK_SIGNING_SECRET"),
//	        Header:    r.Header.Get(webhook.HeaderSignature),
//	        Body:      body,
//	        Tolerance: webhook.DefaultTolerance,
//	    })
//	    if err != nil {
//	        http.Error(w, "invalid signature", http.StatusUnauthorized)
//	        return
//	    }
//
//	    var event map[string]any
//	    if err := json.Unmarshal(body, &event); err != nil {
//	        http.Error(w, "bad json", http.StatusBadRequest)
//	        return
//	    }
//
//	    // Deliveries are at-least-once. Deduplicate on X-Webhook-Id before
//	    // acting on the event; the same id may legitimately arrive twice.
//	    process(r.Header.Get(webhook.HeaderID), event)
//	    w.WriteHeader(http.StatusOK)
//	}
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Headers sent with every delivery.
const (
	// HeaderID is the event's UUID. It is stable across retries, so it is the
	// correct key to deduplicate on.
	HeaderID = "X-Webhook-Id"
	// HeaderTimestamp is the Unix second at which the signature was computed.
	HeaderTimestamp = "X-Webhook-Timestamp"
	// HeaderSignature carries the versioned signature set.
	HeaderSignature = "X-Webhook-Signature"
	// HeaderAttempt is the 1-based delivery attempt number. Present so a
	// receiver can tell a retry from a first delivery; it is NOT part of the
	// signed payload, because a retry of the same event must produce the same
	// digest.
	HeaderAttempt = "X-Webhook-Attempt"
)

// DefaultTolerance is the default replay window. Five minutes is enough to
// absorb ordinary clock skew between two hosts without leaving a captured
// request usable for long.
const DefaultTolerance = 5 * time.Minute

// signatureVersion is the scheme identifier in the header. It exists so the
// digest can be changed later without breaking receivers: a future version adds
// v2= alongside v1=, receivers upgrade, and v1= is then dropped.
const signatureVersion = "v1"

// Errors returned by Verify. They are distinguishable so a receiver can log a
// stale timestamp differently from an outright forgery — the first is usually
// a clock problem, the second is an attack or a misconfigured secret.
var (
	ErrMissingHeader   = errors.New("webhook: signature header is absent")
	ErrMalformedHeader = errors.New("webhook: signature header is malformed")
	ErrNoSignature     = errors.New("webhook: signature header carries no supported version")
	ErrTimestampSkew   = errors.New("webhook: timestamp is outside the tolerance window")
	ErrSignature       = errors.New("webhook: signature does not match")
	ErrNoSecret        = errors.New("webhook: no secret provided")
)

// VerifyParams are the inputs to Verify.
type VerifyParams struct {
	// Secret is the endpoint's signing secret, as returned once by
	// POST /v1/endpoints.
	Secret string
	// Header is the raw X-Webhook-Signature value.
	Header string
	// Body is the exact request body as received, before any parsing.
	Body []byte
	// Tolerance is the maximum age of the timestamp. Zero means
	// DefaultTolerance. A negative value disables the check, which is only
	// appropriate in tests.
	Tolerance time.Duration

	// Now overrides the clock, for tests. Zero means time.Now.
	Now func() time.Time
}

// Verify checks a signature header against a body.
//
// It returns nil only if the header is well formed, the timestamp is inside the
// tolerance window, and at least one signature in the header matches. Comparison
// uses hmac.Equal — a byte-by-byte == would leak, through its own running time,
// how many leading bytes of a forged signature were correct, which is enough to
// reconstruct a valid one.
func Verify(p VerifyParams) error {
	if p.Secret == "" {
		return ErrNoSecret
	}
	if strings.TrimSpace(p.Header) == "" {
		return ErrMissingHeader
	}

	ts, signatures, err := ParseSignatureHeader(p.Header)
	if err != nil {
		return err
	}
	if len(signatures) == 0 {
		return ErrNoSignature
	}

	tolerance := p.Tolerance
	if tolerance == 0 {
		tolerance = DefaultTolerance
	}
	if tolerance > 0 {
		now := time.Now
		if p.Now != nil {
			now = p.Now
		}
		// Absolute difference: a timestamp far in the FUTURE is as suspicious
		// as one far in the past, and only checking the past would let a
		// forged future timestamp stay valid indefinitely.
		if drift := now().Sub(ts); drift > tolerance || drift < -tolerance {
			return fmt.Errorf("%w: %s off by %s", ErrTimestampSkew, ts.Format(time.RFC3339), drift.Round(time.Second))
		}
	}

	expected := computeSignature(p.Secret, ts, p.Body)

	// Every candidate is checked even after a match is found. Returning early
	// would make the loop's duration depend on which signature matched, and a
	// header may legitimately carry several during a secret rotation.
	matched := false
	for _, candidate := range signatures {
		if hmac.Equal(candidate, expected) {
			matched = true
		}
	}
	if !matched {
		return ErrSignature
	}
	return nil
}

// Sign produces the X-Webhook-Signature header value for a body.
//
// The signed payload is "{unix_timestamp}.{raw_body}". Binding the timestamp
// into the digest rather than sending it alongside is what makes the replay
// window enforceable: an attacker who captures a request cannot move its
// timestamp forward without invalidating the signature.
func Sign(secret string, ts time.Time, body []byte) string {
	return fmt.Sprintf("t=%d,%s=%s",
		ts.Unix(),
		signatureVersion,
		hex.EncodeToString(computeSignature(secret, ts, body)),
	)
}

// SignedPayload returns the exact bytes the signature is computed over. Exposed
// so a receiver debugging a mismatch can reproduce it byte for byte.
func SignedPayload(ts time.Time, body []byte) []byte {
	prefix := strconv.FormatInt(ts.Unix(), 10) + "."
	out := make([]byte, 0, len(prefix)+len(body))
	out = append(out, prefix...)
	out = append(out, body...)
	return out
}

func computeSignature(secret string, ts time.Time, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(SignedPayload(ts, body))
	return mac.Sum(nil)
}

// ParseSignatureHeader splits a header into its timestamp and its v1 digests.
//
// Format: "t=1700000000,v1=abc...,v1=def..."
// Multiple v1 values are permitted so a secret can be rotated without an
// outage: the sender signs with both the old and the new secret for a window,
// and receivers accept either.
func ParseSignatureHeader(header string) (time.Time, [][]byte, error) {
	var (
		ts         time.Time
		haveTS     bool
		signatures [][]byte
	)

	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			return time.Time{}, nil, fmt.Errorf("%w: %q is not key=value", ErrMalformedHeader, part)
		}
		switch key {
		case "t":
			secs, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return time.Time{}, nil, fmt.Errorf("%w: timestamp %q is not an integer", ErrMalformedHeader, value)
			}
			ts = time.Unix(secs, 0)
			haveTS = true
		case signatureVersion:
			raw, err := hex.DecodeString(value)
			if err != nil {
				// Skip rather than fail: an unparseable digest among several
				// should not invalidate a header that also carries a good one.
				continue
			}
			signatures = append(signatures, raw)
		default:
			// Unknown versions are ignored so that adding v2 later does not
			// break receivers still on this code.
			continue
		}
	}

	if !haveTS {
		return time.Time{}, nil, fmt.Errorf("%w: no t= element", ErrMalformedHeader)
	}
	return ts, signatures, nil
}
