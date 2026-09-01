package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/singha105/webhook-relay/internal/models"
	"github.com/singha105/webhook-relay/internal/store"
)

// HeaderIdempotencyKey is the optional ingest deduplication header.
const HeaderIdempotencyKey = "Idempotency-Key"

// ingestEvent handles POST /v1/events.
//
// The hot path, and the only endpoint whose latency matters. It does exactly
// four things: decode, validate, write one row, respond. No delivery attempt,
// no endpoint lookup to check existence, no queue publish that could block on
// a second network hop.
//
// Notably absent is a SELECT to confirm the endpoint exists. The foreign key
// already enforces that, so a pre-check would be a second round trip that adds
// latency to every valid request in order to produce a nicer error for an
// invalid one — and it would still be racy against a concurrent delete.
func (s *Server) ingestEvent(w http.ResponseWriter, r *http.Request) {
	var req models.CreateEventRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	endpointID, err := req.Validate()
	if err != nil {
		writeValidationError(w, r, err)
		return
	}

	idempotencyKey := r.Header.Get(HeaderIdempotencyKey)
	if keyErr := models.ValidateIdempotencyKey(idempotencyKey); keyErr != nil {
		writeValidationError(w, r, keyErr)
		return
	}
	// nil, not "", when the header is absent: the partial unique index only
	// covers non-NULL keys, so an empty string would make every un-keyed
	// request collide with every other.
	var keyPtr *string
	if idempotencyKey != "" {
		keyPtr = &idempotencyKey
	}

	eventID, err := models.NewEventID()
	if err != nil {
		writeInternalError(w, r, err, "generate_event_id")
		return
	}

	event, created, err := s.store.CreateEvent(r.Context(), eventID, endpointID, req.EventType, req.Payload, keyPtr)
	if err != nil {
		if errors.Is(err, store.ErrEndpointNotFound) {
			// 422, not 404: the URL is correct, the body references something
			// that does not exist.
			writeError(w, r, http.StatusUnprocessableEntity, CodeEndpointNotFound,
				"endpoint_id does not reference an existing endpoint")
			return
		}
		writeInternalError(w, r, err, "create_event")
		return
	}

	log := LoggerFrom(r.Context()).With(
		slog.String("event_id", event.ID.String()),
		slog.String("endpoint_id", event.EndpointID.String()),
		slog.String("event_type", event.EventType),
	)

	if !created {
		// A replayed idempotency key is a success, not a conflict. 200 rather
		// than 202 tells the caller "this already existed" without making them
		// parse the body to find out.
		log.Info("event ingest deduplicated by idempotency key")
		writeJSON(w, r, http.StatusOK, event)
		return
	}

	log.Info("event accepted")
	// 202, not 201: the event is durably recorded but nothing has been
	// delivered. Claiming 201 Created would imply the work is done.
	writeJSON(w, r, http.StatusAccepted, event)
}

// getEvent handles GET /v1/events/{id}, returning status plus attempt history.
func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r)
	if !ok {
		return
	}
	event, err := s.store.GetEventWithAttempts(r.Context(), id)
	if err != nil {
		if errors.Is(err, models.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, CodeNotFound, "event not found")
			return
		}
		writeInternalError(w, r, err, "get_event")
		return
	}
	writeJSON(w, r, http.StatusOK, event)
}
