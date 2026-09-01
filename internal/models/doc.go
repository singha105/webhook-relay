// Package models holds the domain types shared by every layer of
// webhook-relay, together with the validation rules that guard them.
//
// Nothing in this package touches the database or the network. Validation runs
// before any I/O on the ingest path, so a malformed request costs one JSON
// decode and nothing more. The database re-states the important rules as CHECK
// constraints; those are a backstop against a future code path, not the
// primary defence.
package models
