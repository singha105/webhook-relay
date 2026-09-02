package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/singha105/webhook-relay/internal/models"
	"github.com/singha105/webhook-relay/internal/store"
	"github.com/singha105/webhook-relay/internal/telemetry"
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
	// The root span for an event's entire life. Everything downstream — the
	// queue hop and every delivery attempt — hangs off this one, which is what
	// makes a single trace answer "what happened to event X".
	ctx, span := s.tracer.Start(r.Context(), "webhook.ingest",
		trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()
	r = r.WithContext(ctx)

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

	span.SetAttributes(
		telemetry.AttrEndpointID(endpointID.String()),
		telemetry.AttrEventType(req.EventType),
		telemetry.AttrEventID(eventID.String()),
	)

	event, created, err := s.store.CreateEvent(ctx, eventID, endpointID, req.EventType, req.Payload, keyPtr)
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

	log := LoggerFrom(ctx).With(
		slog.String("event_id", event.ID.String()),
		slog.String("endpoint_id", event.EndpointID.String()),
		slog.String("event_type", event.EventType),
	)
	if traceID := telemetry.TraceIDFrom(ctx); traceID != "" {
		log = log.With(slog.String("trace_id", traceID))
	}

	if !created {
		// A replayed idempotency key is a success, not a conflict. 200 rather
		// than 202 tells the caller "this already existed" without making them
		// parse the body to find out.
		log.Info("event ingest deduplicated by idempotency key")
		span.SetAttributes(attribute.Bool("webhook.deduplicated", true))
		writeJSON(w, r, http.StatusOK, event)
		return
	}

	// Counted only for genuinely new events. Counting replays too would make
	// the ingest rate depend on how often clients retry, which is not what the
	// panel is asking.
	if s.metrics != nil {
		s.metrics.RecordIngest(ctx, event.EventType)
	}
	log.Info("event accepted")
	// 202, not 201: the event is durably recorded but nothing has been
	// delivered. Claiming 201 Created would imply the work is done.
	writeJSON(w, r, http.StatusAccepted, event)
}

// replayEvent handles POST /v1/events/{id}/replay.
//
// Returns a dead-lettered or already-delivered event to the ready set with a
// fresh attempt budget. The attempt history is deliberately kept: a replay is a
// new run of delivery, not a rewriting of what happened, and the operator
// investigating the original failure needs those rows. New attempts continue
// the numbering from where the old ones stopped.
//
// Only terminal states are replayable. Replaying an event that is mid-flight
// would race the worker holding it and could produce two live delivery chains
// for one event, which is exactly the duplicate the rest of the system works to
// avoid.
func (s *Server) replayEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r)
	if !ok {
		return
	}

	event, err := s.store.ReplayEvent(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, models.ErrNotReplayable):
			// 409, not 404: the event exists, its current state just does not
			// permit this transition. A 404 would send an operator hunting for
			// an id that is right there.
			writeError(w, r, http.StatusConflict, CodeNotReplayable,
				"only events in the dlq or delivered state can be replayed")
		case errors.Is(err, models.ErrNotFound):
			writeError(w, r, http.StatusNotFound, CodeNotFound, "event not found")
		default:
			writeInternalError(w, r, err, "replay_event")
		}
		return
	}

	log := LoggerFrom(r.Context()).With(
		slog.String("event_id", event.ID.String()),
		slog.String("endpoint_id", event.EndpointID.String()),
	)

	// Best effort: push it straight to the stream so an operator watching sees
	// it move immediately. If this fails the relay still picks it up on its
	// next poll, so the failure is logged and not returned.
	if s.queue != nil {
		if err := s.queue.Enqueue(r.Context(), event.ID); err != nil {
			log.Warn("replay enqueued via the relay instead of directly",
				slog.Any("error", err))
		}
	}

	log.Info("event replayed; attempt budget reset")
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
