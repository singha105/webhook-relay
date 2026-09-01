package main

import (
	"github.com/singha105/webhook-relay/test/sink"
)

// newSink builds the sink, optionally verifying signatures.
func newSink(secret string) *sink.Sink {
	s := sink.New()
	if secret != "" {
		s.SetSecret(secret)
	}
	return s
}
