// Package queue is the work-transport seam between the outbox relay and the
// delivery workers.
//
// # Why Valkey Streams
//
// A stream with a consumer group is the only one of the three obvious options
// that can express "this worker is holding this job right now, and if it dies
// somebody else must get it."
//
//   - A plain LIST is out. LPOP hands the job to a worker and forgets it
//     immediately. If that worker is killed between the pop and the HTTP call —
//     which is precisely what Day 5 does — the job is gone with no record that
//     it ever existed. That breaks at-least-once, which is the entire promise
//     of this service. BRPOPLPUSH into a per-worker processing list gets you
//     part of the way, but then recovery means enumerating every worker's list
//     and reasoning about which owners are still alive: a consumer group
//     rebuilt by hand, badly.
//
//   - Postgres SELECT ... FOR UPDATE SKIP LOCKED is genuinely tempting, and it
//     is what the outbox relay itself uses. It would let us delete Valkey
//     outright and make enqueue transactional with the insert. It loses on two
//     counts. Each polling worker holds a connection for the duration of its
//     transaction, so worker count is bounded by max_connections rather than by
//     anything about the workload. And queue churn would land on the same disk
//     as the durable event log — the one thing we least want to saturate under
//     the Day 6 load test.
//
//   - Streams keep a per-group Pending Entries List. XREADGROUP moves an entry
//     into the PEL and it stays there, attributed to a named consumer, until
//     XACK. XAUTOCLAIM reassigns entries that have sat idle too long. That is
//     exactly the "worker died holding a job" recovery we need, as a primitive
//     rather than as something we build.
//
// # What we give up
//
//   - Enqueue is not transactional with the Postgres insert. Two systems, one
//     crash window. That is why ingest never writes here directly; see
//     internal/relay for the outbox that closes it.
//   - The PEL is memory-resident and grows with the number of unacknowledged
//     entries. A worker pool that dies without acking leaves entries that only
//     XAUTOCLAIM or trimming reclaims.
//   - No delayed delivery. Streams cannot say "deliver this in four minutes,"
//     so backoff scheduling lives in Postgres (events.next_retry_at) and the
//     relay only enqueues work that is due now.
//   - At-least-once, never exactly-once. A worker can deliver and then die
//     before XACK, and the entry will be reclaimed and delivered again. The
//     delivery-side dedup key in internal/delivery narrows that window; it
//     cannot close it, because the receiver is across a network boundary.
package queue

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrEmpty is returned by Claim when no work is available.
var ErrEmpty = errors.New("queue is empty")

// ClaimedMessage is one unit of delivery work held by a consumer.
//
// It carries only the event ID. The payload, the endpoint, and the current
// attempt count are all read from Postgres at delivery time, so a message that
// has been sitting in the stream — or that is being redelivered after a
// reclaim — cannot act on a stale copy of a row that has since changed. The
// cost is one indexed read per delivery, which is cheap next to an outbound
// HTTP call.
type ClaimedMessage struct {
	// MessageID is the stream entry ID, opaque to callers. It is what Ack and
	// Nack address, and it is not the event ID: the same event redelivered
	// after a reclaim arrives under a new message.
	MessageID string

	// EventID identifies the row in events.
	EventID uuid.UUID

	// DeliveryCount is how many times this entry has been handed to a consumer,
	// including this one. It starts at 1. A value above 1 means a previous
	// consumer took the entry and never acked it — useful for distinguishing
	// "this endpoint keeps returning 500" from "our workers keep dying holding
	// this job", which are very different incidents.
	DeliveryCount int64

	// TraceFields carries W3C trace context from the producer, so a delivery
	// span joins the trace that began at ingest instead of starting a new one.
	// Empty for messages enqueued before tracing was enabled, which is handled
	// by starting a fresh root rather than failing.
	TraceFields map[string]string
}

// Queue is the contract between the outbox relay and the delivery workers.
//
// Implementations must be at-least-once: an entry that is claimed but never
// acknowledged has to become claimable again.
type Queue interface {
	// Enqueue makes an event available to consumers. It must be safe to call
	// twice with the same event ID — the relay can crash after enqueueing and
	// before recording that it did, so duplicates are expected by design.
	//
	// Implementations inject the caller's trace context into the message, so
	// the delivery that eventually happens is part of the same trace.
	Enqueue(ctx context.Context, eventID uuid.UUID) error

	// Claim takes up to count entries for the named consumer. It returns
	// ErrEmpty rather than blocking indefinitely, so the caller keeps control
	// of its own poll cadence and can shut down promptly.
	Claim(ctx context.Context, consumerID string, count int) ([]ClaimedMessage, error)

	// Ack marks an entry as processed and removes it from the pending list.
	// Called for terminal outcomes and for scheduled retries alike: once the
	// retry is durably recorded in Postgres, the stream entry has done its job.
	Ack(ctx context.Context, messageID string) error

	// Nack returns an entry for redelivery. retryAfter is advisory — Streams
	// have no delayed redelivery, so an implementation may only be able to make
	// the entry immediately reclaimable. Callers that need a real delay must
	// schedule it in Postgres and Ack instead.
	Nack(ctx context.Context, messageID string, retryAfter time.Duration) error

	// ReclaimStale returns entries that have been held longer than idleTimeout
	// without an ack, reassigning them to the calling consumer. This is the
	// recovery path for a worker that died mid-delivery.
	ReclaimStale(ctx context.Context, consumerID string, idleTimeout time.Duration, count int) ([]ClaimedMessage, error)

	// Depth reports entries not yet delivered to any consumer.
	Depth(ctx context.Context) (int64, error)

	// Len reports the total number of entries in the stream, delivered or not.
	Len(ctx context.Context) (int64, error)

	// Pending reports entries claimed but not yet acknowledged.
	Pending(ctx context.Context) (int64, error)

	// Ping verifies connectivity, for the readiness probe.
	Ping(ctx context.Context) error

	// Close releases resources.
	Close() error
}
