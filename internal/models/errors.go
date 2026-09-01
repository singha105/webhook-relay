package models

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is returned by the store when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// FieldError describes a single failed validation rule.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidationError aggregates every field that failed validation, so a caller
// fixing a bad request sees all of its problems at once instead of discovering
// them one round-trip at a time.
type ValidationError struct {
	Fields []FieldError `json:"errors"`
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Error())
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// add records a failed rule. It is a no-op guard-free helper used by the
// Validate methods below.
func (e *ValidationError) add(field, msg string) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: msg})
}

// orNil returns nil when nothing failed, so callers can write
// `return v.orNil()` without an explicit length check at every site.
func (e *ValidationError) orNil() error {
	if len(e.Fields) == 0 {
		return nil
	}
	return e
}
