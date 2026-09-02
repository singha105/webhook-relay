package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/singha105/webhook-relay/internal/models"
)

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// createEndpoint handles POST /v1/endpoints.
func (s *Server) createEndpoint(w http.ResponseWriter, r *http.Request) {
	var req models.CreateEndpointRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeValidationError(w, r, err)
		return
	}

	secret, err := models.GenerateSigningSecret()
	if err != nil {
		writeInternalError(w, r, err, "generate_signing_secret")
		return
	}

	endpoint, err := s.store.CreateEndpoint(r.Context(), req.URL, req.Description, secret, *req.RateLimitPerSec)
	if err != nil {
		writeInternalError(w, r, err, "create_endpoint")
		return
	}

	// The one and only time the secret leaves the service. Logged by id, never
	// by value.
	LoggerFrom(r.Context()).Info("endpoint registered",
		slog.String("endpoint_id", endpoint.ID.String()),
		slog.String("url", endpoint.URL),
	)

	writeJSON(w, r, http.StatusCreated, models.CreateEndpointResponse{
		Endpoint:          *endpoint,
		SigningSecretOnce: secret,
	})
}

// listEndpoints handles GET /v1/endpoints.
func (s *Server) listEndpoints(w http.ResponseWriter, r *http.Request) {
	limit := clampQueryInt(r, "limit", defaultListLimit, 1, maxListLimit)
	offset := clampQueryInt(r, "offset", 0, 0, 1<<30)

	endpoints, err := s.store.ListEndpoints(r.Context(), limit, offset)
	if err != nil {
		writeInternalError(w, r, err, "list_endpoints")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"endpoints": endpoints,
		"limit":     limit,
		"offset":    offset,
	})
}

// getEndpoint handles GET /v1/endpoints/{id}.
func (s *Server) getEndpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r)
	if !ok {
		return
	}
	endpoint, err := s.store.GetEndpoint(r.Context(), id)
	if err != nil {
		if errors.Is(err, models.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, CodeNotFound, "endpoint not found")
			return
		}
		writeInternalError(w, r, err, "get_endpoint")
		return
	}

	// The breaker state is read live rather than stored, and a failure to read
	// it leaves the field absent rather than defaulting to "closed" — an
	// operator must be able to tell "this endpoint is healthy" from "we could
	// not find out".
	if s.breaker != nil {
		state, brkErr := s.breaker.Current(r.Context(), id)
		if brkErr != nil {
			LoggerFrom(r.Context()).Warn("could not read circuit breaker state",
				slog.String("endpoint_id", id.String()), slog.Any("error", brkErr))
		} else {
			endpoint.CircuitBreakerState = string(state)
		}
	}

	writeJSON(w, r, http.StatusOK, endpoint)
}

// updateEndpoint handles PATCH /v1/endpoints/{id}.
func (s *Server) updateEndpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r)
	if !ok {
		return
	}
	var req models.UpdateEndpointRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeValidationError(w, r, err)
		return
	}

	endpoint, err := s.store.UpdateEndpoint(r.Context(), id, req)
	if err != nil {
		if errors.Is(err, models.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, CodeNotFound, "endpoint not found")
			return
		}
		writeInternalError(w, r, err, "update_endpoint")
		return
	}
	writeJSON(w, r, http.StatusOK, endpoint)
}

// deleteEndpoint handles DELETE /v1/endpoints/{id}.
func (s *Server) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteEndpoint(r.Context(), id); err != nil {
		if errors.Is(err, models.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, CodeNotFound, "endpoint not found")
			return
		}
		writeInternalError(w, r, err, "delete_endpoint")
		return
	}
	// Deleting an endpoint cascades to its events and their attempts. Worth a
	// log line: it is the most destructive operation this API offers.
	LoggerFrom(r.Context()).Warn("endpoint deleted, cascading to its events",
		slog.String("endpoint_id", id.String()),
	)
	w.WriteHeader(http.StatusNoContent)
}

// parseIDParam reads the {id} URL parameter as a UUID, answering 404 rather
// than 400 on a malformed value: from the client's perspective
// "/v1/endpoints/garbage" is a URL that addresses nothing, and 400 would leak
// that the segment is parsed as a UUID.
//
// Every route in this API names its identifier "id", so the parameter name is
// fixed rather than passed in.
func parseIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := chi.URLParam(r, "id")
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "not found")
		return uuid.Nil, false
	}
	return id, true
}

// clampQueryInt reads an integer query parameter, silently clamping to range.
// A malformed or out-of-range paging parameter falls back to the default
// rather than failing the request, because there is no useful action a client
// can take on "limit=abc" that it could not take on a sane default.
func clampQueryInt(r *http.Request, name string, def, minValue, maxValue int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < minValue {
		return minValue
	}
	if n > maxValue {
		return maxValue
	}
	return n
}

// decodeJSON reads and validates the request body, writing the error response
// itself and reporting whether the caller should continue.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	// Reject unknown fields: a client that misspells "rate_limit_per_sec"
	// should be told, not silently given the default.
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, r, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "request body is too large")
			return false
		}
		writeError(w, r, http.StatusBadRequest, CodeMalformedJSON, "request body is not valid JSON: "+err.Error())
		return false
	}
	// A second value in the stream means the client sent concatenated JSON,
	// which is almost always a bug worth surfacing.
	if dec.More() {
		writeError(w, r, http.StatusBadRequest, CodeMalformedJSON, "request body must contain exactly one JSON object")
		return false
	}
	return true
}
