// Package queue defines the work queue contract between the ingest API and the
// delivery worker.
//
// Day 1 ships the interface with no implementation. That is deliberate: the
// API persists events with status 'pending' and nothing consumes them yet, so
// there is no behaviour to get wrong. Defining the seam now means Day 2 adds a
// Valkey implementation behind an interface the API already compiles against,
// rather than reshaping the API around whatever the queue turns out to need.
//
// Planned implementation (Day 2): Valkey Streams with consumer groups, not a
// plain LIST. A LIST pops a job into the void — if the worker dies mid-flight
// the job is simply gone, which would break the at-least-once guarantee this
// service is built to make. Streams keep a pending-entries list per consumer
// group, so an unacknowledged job can be reclaimed by another worker via
// XAUTOCLAIM. The cost is a heavier data structure and having to run a
// reclaimer for abandoned entries.
//
// Postgres SELECT ... FOR UPDATE SKIP LOCKED was the alternative. It would let
// us drop Valkey entirely and get transactional enqueue for free, which is
// genuinely attractive. We are not doing it because polling Postgres burns a
// connection per worker per poll and puts queue depth on the same disk as the
// durable event log — the thing we least want to saturate under load. Day 6's
// benchmark is the honest test of that call; if Streams do not earn their
// keep, this interface is the seam that makes swapping back cheap.
package queue

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrEmpty is returned by Dequeue when no work is available.
var ErrEmpty = errors.New("queue is empty")

// Job is a unit of delivery work. It carries only identifiers: the payload
// lives in Postgres, and re-reading it at delivery time means the queue never
// holds a stale copy of a row that has since been updated.
type Job struct {
	EventID    uuid.UUID
	EndpointID uuid.UUID
	// Attempt is the delivery attempt this job represents, starting at 1.
	Attempt int
	// ReceiptID identifies this specific delivery of the job, for Ack. It is
	// opaque to callers; the Valkey implementation will carry a stream entry ID.
	ReceiptID string
}

// Queue is the contract the delivery worker consumes.
//
// Implementations must be at-least-once: a job that is dequeued but never
// acknowledged has to become visible to another consumer again. Exactly-once
// is not offered, because it is not achievable across a network boundary to a
// third-party endpoint — the endpoint itself must tolerate duplicates, which
// is why every delivery carries a stable event ID for the receiver to
// deduplicate on.
type Queue interface {
	// Enqueue makes a job available to consumers. It must be safe to call
	// twice with the same EventID; duplicate delivery is permitted by the
	// at-least-once contract.
	Enqueue(ctx context.Context, job Job) error

	// Dequeue claims up to n jobs for this consumer. It returns ErrEmpty when
	// nothing is available rather than blocking indefinitely, so the caller
	// keeps control of its own poll cadence and shutdown.
	Dequeue(ctx context.Context, consumer string, n int) ([]Job, error)

	// Ack marks a job as successfully processed, removing it from the pending
	// set. A job that is never acked must eventually be redelivered.
	Ack(ctx context.Context, job Job) error

	// Nack returns a job for immediate redelivery, for the case where a worker
	// knows it cannot proceed. Backoff scheduling is the caller's concern, not
	// the queue's.
	Nack(ctx context.Context, job Job) error

	// Depth reports how many jobs are waiting. Used by the readiness probe and
	// exported as a metric on Day 4.
	Depth(ctx context.Context) (int64, error)

	// Close releases resources held by the implementation.
	Close() error
}
