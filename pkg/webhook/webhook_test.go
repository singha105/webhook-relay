package webhook_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/singha105/webhook-relay/pkg/webhook"
)

const testSecret = "whsec_test_do_not_use_in_production"

func TestSignAndVerifyRoundTrip(t *testing.T) {
	body := []byte(`{"event":"order.created","id":123}`)
	ts := time.Now()

	header := webhook.Sign(testSecret, ts, body)

	if err := webhook.Verify(webhook.VerifyParams{
		Secret: testSecret,
		Header: header,
		Body:   body,
	}); err != nil {
		t.Fatalf("Verify() = %v, want nil", err)
	}
}

func TestSignHeaderFormat(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	got := webhook.Sign(testSecret, ts, []byte("{}"))

	if !strings.HasPrefix(got, "t=1700000000,v1=") {
		t.Errorf("header = %q, want t=<unix>,v1=<hex>", got)
	}
	_, sigs, err := webhook.ParseSignatureHeader(got)
	if err != nil {
		t.Fatalf("ParseSignatureHeader() = %v", err)
	}
	// SHA-256 produces 32 bytes.
	if len(sigs) != 1 || len(sigs[0]) != 32 {
		t.Errorf("got %d signatures of length %d, want 1 of 32", len(sigs), len(sigs[0]))
	}
}

// The signature is over "{timestamp}.{raw_body}", so anything that changes
// either half must invalidate it.
func TestVerifyRejectsTampering(t *testing.T) {
	body := []byte(`{"amount":100}`)
	ts := time.Now()
	header := webhook.Sign(testSecret, ts, body)

	tests := []struct {
		name   string
		secret string
		header string
		body   []byte
		want   error
	}{
		{
			name:   "body modified",
			secret: testSecret,
			header: header,
			body:   []byte(`{"amount":999999}`),
			want:   webhook.ErrSignature,
		},
		{
			name:   "body reordered — same JSON, different bytes",
			secret: testSecret,
			header: header,
			// Semantically identical to nothing here, but the point stands:
			// verification is over bytes, not over parsed structure.
			body: []byte(`{"amount": 100}`),
			want: webhook.ErrSignature,
		},
		{
			name:   "wrong secret",
			secret: "whsec_attacker_guess",
			header: header,
			body:   body,
			want:   webhook.ErrSignature,
		},
		{
			name:   "timestamp moved, signature not recomputed",
			secret: testSecret,
			header: fmt.Sprintf("t=%d,%s", ts.Add(time.Minute).Unix(), strings.SplitN(header, ",", 2)[1]),
			body:   body,
			want:   webhook.ErrSignature,
		},
		{
			name:   "empty secret",
			secret: "",
			header: header,
			body:   body,
			want:   webhook.ErrNoSecret,
		},
		{
			name:   "absent header",
			secret: testSecret,
			header: "",
			body:   body,
			want:   webhook.ErrMissingHeader,
		},
		{
			name:   "whitespace header",
			secret: testSecret,
			header: "   ",
			body:   body,
			want:   webhook.ErrMissingHeader,
		},
		{
			name:   "header with no t=",
			secret: testSecret,
			header: "v1=abcdef",
			body:   body,
			want:   webhook.ErrMalformedHeader,
		},
		{
			name:   "header that is not key=value",
			secret: testSecret,
			header: "garbage",
			body:   body,
			want:   webhook.ErrMalformedHeader,
		},
		{
			name:   "non-integer timestamp",
			secret: testSecret,
			header: "t=not-a-number,v1=abcdef",
			body:   body,
			want:   webhook.ErrMalformedHeader,
		},
		{
			name:   "no supported signature version",
			secret: testSecret,
			header: "t=1700000000,v9=abcdef",
			body:   body,
			want:   webhook.ErrNoSignature,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := webhook.Verify(webhook.VerifyParams{
				Secret:    tt.secret,
				Header:    tt.header,
				Body:      tt.body,
				Tolerance: -1, // skew checked separately
			})
			if !errors.Is(err, tt.want) {
				t.Errorf("Verify() = %v, want %v", err, tt.want)
			}
		})
	}
}

// A captured request must stop being replayable. The timestamp is inside the
// digest, so an attacker cannot simply move it forward.
func TestVerifyTimestampTolerance(t *testing.T) {
	body := []byte(`{"a":1}`)
	base := time.Unix(1700000000, 0)
	header := webhook.Sign(testSecret, base, body)

	tests := []struct {
		name      string
		now       time.Time
		tolerance time.Duration
		wantErr   error
	}{
		{"exactly now", base, time.Minute, nil},
		{"just inside the window", base.Add(4 * time.Minute), 5 * time.Minute, nil},
		{"just outside the window", base.Add(6 * time.Minute), 5 * time.Minute, webhook.ErrTimestampSkew},
		{"long expired", base.Add(24 * time.Hour), webhook.DefaultTolerance, webhook.ErrTimestampSkew},
		// A future timestamp is as suspicious as a stale one: accepting it
		// would leave a forged request valid indefinitely.
		{"implausibly far in the future", base.Add(-6 * time.Minute), 5 * time.Minute, webhook.ErrTimestampSkew},
		{"tolerance disabled", base.Add(365 * 24 * time.Hour), -1, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := tt.now
			err := webhook.Verify(webhook.VerifyParams{
				Secret:    testSecret,
				Header:    header,
				Body:      body,
				Tolerance: tt.tolerance,
				Now:       func() time.Time { return now },
			})
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("Verify() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Verify() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// During a secret rotation the sender signs with both secrets for a window.
func TestVerifyAcceptsMultipleSignatures(t *testing.T) {
	body := []byte(`{"rotate":true}`)
	ts := time.Now()

	oldHeader := webhook.Sign("whsec_old_secret", ts, body)
	newHeader := webhook.Sign("whsec_new_secret", ts, body)

	// "t=...,v1=<old>,v1=<new>"
	combined := oldHeader + "," + strings.SplitN(newHeader, ",", 2)[1]

	for _, secret := range []string{"whsec_old_secret", "whsec_new_secret"} {
		t.Run("accepts "+secret, func(t *testing.T) {
			if err := webhook.Verify(webhook.VerifyParams{
				Secret: secret, Header: combined, Body: body,
			}); err != nil {
				t.Errorf("Verify() = %v, want nil", err)
			}
		})
	}

	t.Run("still rejects a third secret", func(t *testing.T) {
		err := webhook.Verify(webhook.VerifyParams{
			Secret: "whsec_wrong", Header: combined, Body: body,
		})
		if !errors.Is(err, webhook.ErrSignature) {
			t.Errorf("Verify() = %v, want ErrSignature", err)
		}
	})
}

// A malformed digest alongside a good one must not invalidate the header.
func TestVerifyIgnoresUnparseableSignatures(t *testing.T) {
	body := []byte(`{"a":1}`)
	ts := time.Now()
	good := webhook.Sign(testSecret, ts, body)
	mixed := good + ",v1=zzzznothex,v2=futureversion"

	if err := webhook.Verify(webhook.VerifyParams{
		Secret: testSecret, Header: mixed, Body: body,
	}); err != nil {
		t.Errorf("Verify() = %v, want nil", err)
	}
}

func TestSignedPayloadShape(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	got := string(webhook.SignedPayload(ts, []byte(`{"a":1}`)))
	want := `1700000000.{"a":1}`
	if got != want {
		t.Errorf("SignedPayload() = %q, want %q", got, want)
	}
}

func TestSignIsDeterministic(t *testing.T) {
	// A retry of the same event must produce an identical digest, or a
	// receiver deduplicating on signature would see every retry as new.
	body := []byte(`{"a":1}`)
	ts := time.Unix(1700000000, 0)
	if webhook.Sign(testSecret, ts, body) != webhook.Sign(testSecret, ts, body) {
		t.Error("Sign() is not deterministic")
	}
}

func TestVerifyEmptyBody(t *testing.T) {
	ts := time.Now()
	for _, body := range [][]byte{nil, {}} {
		header := webhook.Sign(testSecret, ts, body)
		if err := webhook.Verify(webhook.VerifyParams{
			Secret: testSecret, Header: header, Body: body,
		}); err != nil {
			t.Errorf("Verify() with empty body = %v, want nil", err)
		}
	}
}
