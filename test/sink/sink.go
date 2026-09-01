// Package sink is a controllable webhook receiver used by the integration
// tests and by the manual Day 2 verification.
//
// It is deliberately not a mock. It is a real HTTP server that a real
// http.Client connects to over a real socket, so the things most likely to
// break in delivery — connection reuse, timeouts, header handling, signature
// bytes — are exercised rather than stubbed.
//
// Behaviour is mutable at runtime through a control API, because the Day 2
// verification requires flipping an endpoint from failing to healthy while the
// worker is running. Restarting the sink would give it a new port and lose the
// recorded history, which is exactly the evidence we want to keep.
package sink

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/singha105/webhook-relay/pkg/webhook"
)

// Behavior controls how the sink responds.
type Behavior struct {
	// Status is returned when the request is not selected to fail. Default 200.
	Status int `json:"status"`
	// FailPercent is the chance, 0-100, of returning FailStatus instead.
	FailPercent int `json:"fail_percent"`
	// FailStatus is returned on a simulated failure. Default 500.
	FailStatus int `json:"fail_status"`
	// Delay is slept before responding, for exercising client timeouts.
	Delay Duration `json:"delay"`
	// RetryAfter, when set, is sent as the Retry-After header.
	RetryAfter string `json:"retry_after"`
	// FailFirstN makes the first N requests for each event fail, then succeed.
	// This is how "retry then success" is tested deterministically, without
	// depending on a random draw.
	FailFirstN int `json:"fail_first_n"`
}

// Duration is a time.Duration that round-trips through JSON as a string like
// "2s", so the control API is usable from curl.
type Duration time.Duration

// MarshalJSON renders the duration as a string like "2s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either a duration string ("2s") or a raw nanosecond
// count, so the control API is usable from curl without knowing which.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n int64
		if err2 := json.Unmarshal(b, &n); err2 == nil {
			*d = Duration(time.Duration(n))
			return nil
		}
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("sink: %q is not a duration", s)
	}
	*d = Duration(parsed)
	return nil
}

// Record is one received delivery.
type Record struct {
	EventID    string    `json:"event_id"`
	Attempt    int       `json:"attempt"`
	ReceivedAt time.Time `json:"received_at"`
	Body       string    `json:"body"`
	Signature  string    `json:"signature"`
	// SignatureValid is nil when the sink has no secret configured, so a
	// verified delivery is distinguishable from an unchecked one.
	SignatureValid *bool  `json:"signature_valid,omitempty"`
	Status         int    `json:"status_returned"`
	Note           string `json:"note,omitempty"`
}

// Sink is a recording, controllable webhook receiver.
type Sink struct {
	mu       sync.Mutex
	behavior Behavior
	records  []Record
	// secret enables signature verification. Empty disables it.
	secret string
	// perEvent counts requests per event id, for FailFirstN.
	perEvent map[string]int
}

// New returns a sink that answers 200 and records everything.
func New() *Sink {
	return &Sink{
		behavior: Behavior{Status: http.StatusOK, FailStatus: http.StatusInternalServerError},
		perEvent: make(map[string]int),
	}
}

// SetSecret enables signature verification against secret.
func (s *Sink) SetSecret(secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secret = secret
}

// SetBehavior replaces the current behaviour.
func (s *Sink) SetBehavior(b Behavior) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.Status == 0 {
		b.Status = http.StatusOK
	}
	if b.FailStatus == 0 {
		b.FailStatus = http.StatusInternalServerError
	}
	s.behavior = b
}

// Behavior returns the current behaviour.
func (s *Sink) Behavior() Behavior {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.behavior
}

// Records returns a copy of everything received.
func (s *Sink) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.records))
	copy(out, s.records)
	return out
}

// Reset clears the history and the per-event counters, leaving behaviour alone.
func (s *Sink) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = nil
	s.perEvent = make(map[string]int)
}

// Count returns how many deliveries have been received.
func (s *Sink) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// CountFor returns how many deliveries were received for one event.
func (s *Sink) CountFor(eventID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.records {
		if r.EventID == eventID {
			n++
		}
	}
	return n
}

// Duplicates returns event ids that were delivered more than once for the SAME
// attempt number.
//
// This is the signal the delivery dedup guard exists to suppress. A repeated
// event id across DIFFERENT attempt numbers is an ordinary retry and is not
// reported here; only the same attempt arriving twice means a message was
// reclaimed and redelivered after it had already been dispatched.
func (s *Sink) Duplicates() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]int, len(s.records))
	for _, r := range s.records {
		seen[fmt.Sprintf("%s:%d", r.EventID, r.Attempt)]++
	}
	dupes := make(map[string]int)
	for key, n := range seen {
		if n > 1 {
			dupes[key] = n
		}
	}
	return dupes
}

// Handler returns the sink's HTTP handler: the webhook endpoint plus a control
// API under /_control.
func (s *Sink) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/_control/behavior", s.handleBehavior)
	mux.HandleFunc("/_control/records", s.handleRecords)
	mux.HandleFunc("/_control/reset", s.handleReset)
	mux.HandleFunc("/_control/stats", s.handleStats)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Everything else is a delivery target, so a test can use any path.
	mux.HandleFunc("/", s.handleWebhook)
	return mux
}

func (s *Sink) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "sink accepts POST", http.StatusMethodNotAllowed)
		return
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	defer func() { _ = r.Body.Close() }()

	eventID := r.Header.Get(webhook.HeaderID)
	attempt := 0
	if _, err := fmt.Sscanf(r.Header.Get(webhook.HeaderAttempt), "%d", &attempt); err != nil {
		attempt = 0
	}
	signature := r.Header.Get(webhook.HeaderSignature)

	s.mu.Lock()
	behavior := s.behavior
	secret := s.secret
	s.perEvent[eventID]++
	seenForEvent := s.perEvent[eventID]
	s.mu.Unlock()

	// Signature verification happens on the raw bytes, before any parsing —
	// the same thing the package doc tells real receivers to do.
	var valid *bool
	var note string
	if secret != "" {
		err := webhook.Verify(webhook.VerifyParams{
			Secret: secret, Header: signature, Body: body,
		})
		ok := err == nil
		valid = &ok
		if err != nil {
			note = err.Error()
		}
	}

	if d := time.Duration(behavior.Delay); d > 0 {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			// The client gave up. Record it so a timeout test can prove the
			// request actually arrived.
			s.record(Record{
				EventID: eventID, Attempt: attempt, ReceivedAt: time.Now(),
				Body: string(body), Signature: signature, SignatureValid: valid,
				Status: 0, Note: "client disconnected before the sink responded",
			})
			return
		}
	}

	status := behavior.Status
	switch {
	case behavior.FailFirstN > 0 && seenForEvent <= behavior.FailFirstN:
		status = behavior.FailStatus
	case behavior.FailPercent > 0 && rand.IntN(100) < behavior.FailPercent:
		status = behavior.FailStatus
	}

	if behavior.RetryAfter != "" && (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) {
		w.Header().Set("Retry-After", behavior.RetryAfter)
	}

	s.record(Record{
		EventID: eventID, Attempt: attempt, ReceivedAt: time.Now(),
		Body: string(body), Signature: signature, SignatureValid: valid,
		Status: status, Note: note,
	})

	// Encoded rather than built with Fprintf: eventID comes from a request
	// header, so hand-assembling JSON around it would let a caller inject
	// structure into the response body.
	writeJSONStatus(w, status, map[string]any{"received": eventID, "status": status})
}

func (s *Sink) record(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
}

func (s *Sink) handleBehavior(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.Behavior())
	case http.MethodPost, http.MethodPut:
		var b Behavior
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.SetBehavior(b)
		writeJSON(w, s.Behavior())
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func (s *Sink) handleRecords(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Records())
}

func (s *Sink) handleReset(w http.ResponseWriter, _ *http.Request) {
	s.Reset()
	writeJSON(w, map[string]string{"status": "reset"})
}

func (s *Sink) handleStats(w http.ResponseWriter, _ *http.Request) {
	records := s.Records()
	byEvent := make(map[string]int)
	for _, r := range records {
		byEvent[r.EventID]++
	}
	writeJSON(w, map[string]any{
		"total":            len(records),
		"distinct_events":  len(byEvent),
		"per_event":        byEvent,
		"duplicate_sends":  s.Duplicates(),
		"current_behavior": s.Behavior(),
	})
}

// writeJSON writes a 200 response. Control-API handlers only ever succeed or
// fail with http.Error, so a status parameter would always receive 200.
func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

// writeJSONStatus writes a JSON body with an explicit status, which the webhook
// handler needs because the whole point is returning the configured code.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
