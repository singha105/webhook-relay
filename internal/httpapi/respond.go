package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/singha105/webhook-relay/internal/models"
)

// ErrorCode is a stable, machine-readable error identifier. Clients branch on
// this; the human-readable message is free to change without breaking them.
type ErrorCode string

const (
	CodeValidationFailed ErrorCode = "validation_failed"
	CodeMalformedJSON    ErrorCode = "malformed_json"
	CodeNotFound         ErrorCode = "not_found"
	CodeEndpointNotFound ErrorCode = "endpoint_not_found"
	CodeInternal         ErrorCode = "internal_error"
	CodeMethodNotAllowed ErrorCode = "method_not_allowed"
	CodePayloadTooLarge  ErrorCode = "payload_too_large"
)

// ErrorBody is the single error shape every failure uses. One shape means a
// client writes one error handler.
type ErrorBody struct {
	Code    ErrorCode           `json:"code"`
	Message string              `json:"message"`
	Fields  []models.FieldError `json:"fields,omitempty"`
	// RequestID lets a user paste one value into a support request and have it
	// match a log line exactly.
	RequestID string `json:"request_id,omitempty"`
}

type errorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// writeJSON serializes v as JSON with the given status.
//
// Encoding into a buffer first would let us fail before writing a header; we
// deliberately do not, because these payloads are small and the alternative
// costs an allocation per response. If encoding fails after the header is
// written the connection breaks, which is the honest outcome — a truncated
// body is better than a 200 that lies.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		LoggerFrom(r.Context()).Error("failed to encode response body", slog.Any("error", err))
	}
}

// writeError emits the standard error envelope.
func writeError(w http.ResponseWriter, r *http.Request, status int, code ErrorCode, message string, fields ...models.FieldError) {
	writeJSON(w, r, status, errorEnvelope{Error: ErrorBody{
		Code:      code,
		Message:   message,
		Fields:    fields,
		RequestID: RequestIDFrom(r.Context()),
	}})
}

// writeValidationError turns a *models.ValidationError into a 400 listing every
// broken field.
func writeValidationError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *models.ValidationError
	if errors.As(err, &ve) {
		writeError(w, r, http.StatusBadRequest, CodeValidationFailed, "the request body failed validation", ve.Fields...)
		return
	}
	writeError(w, r, http.StatusBadRequest, CodeValidationFailed, err.Error())
}

// writeInternalError logs the real cause and returns a generic message.
// Internal errors frequently embed SQL text, table names, or connection
// strings; none of that belongs in a response body.
func writeInternalError(w http.ResponseWriter, r *http.Request, err error, op string) {
	LoggerFrom(r.Context()).Error("request failed",
		slog.String("op", op),
		slog.Any("error", err),
	)
	writeError(w, r, http.StatusInternalServerError, CodeInternal, "an internal error occurred")
}
