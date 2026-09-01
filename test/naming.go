package test

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

var schemaCounter atomic.Uint64

// uniqueSchemaName derives a Postgres-safe schema name from the test name.
// Subtest names contain slashes and spaces, and Postgres identifiers are
// truncated at 63 bytes, so we sanitize and append a counter to guarantee
// uniqueness even after truncation.
func uniqueSchemaName(t *testing.T) string {
	t.Helper()

	safe := sanitizeIdent(t.Name())

	const maxBase = 40
	if len(safe) > maxBase {
		safe = safe[:maxBase]
	}
	return fmt.Sprintf("t_%s_%d", safe, schemaCounter.Add(1))
}

// sanitizeIdent lowercases a test name and replaces every character that is not
// alphanumeric or an underscore, so the result is safe as a Postgres identifier
// or a Valkey key segment. Subtest names contain slashes and spaces, both of
// which would otherwise need quoting in one context and break parsing in the
// other.
func sanitizeIdent(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32 // lowercase
		default:
			return '_'
		}
	}, name)
}
