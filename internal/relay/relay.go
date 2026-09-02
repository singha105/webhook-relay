// Package relay implements the transactional outbox: the bridge from durable
// events in Postgres to deliverable work in the queue.
//
// # Why an outbox at all
//
// Ingest has to do two things — persist the event and make it deliverable —
// and they live in two systems. There is no transaction spanning both, so
// whatever order they are attempted in, a crash between them leaves an
// inconsistency:
//
//   - Enqueue first, then insert: a crash in between puts a message in the
//     stream referring to an event that does not exist. The worker loads it,
//     finds nothing, and has to decide whether that is a bug or a race.
//   - Insert first, then enqueue: a crash in between leaves an event that is
//     durably recorded and that nothing will ever deliver. Silent data loss,
//     the worst of the options, because nothing reports an error.
//
// The outbox removes the choice. Ingest writes ONLY to Postgres, in one
// transaction, and returns. A separate relay is the sole producer for the
// queue, and it derives what to enqueue from the durable rows. A crash can then
// only cause a duplicate enqueue — never a lost event — and duplicates are
// something the system already has to tolerate under at-least-once delivery.
//
// The cost is latency: an event waits up to one poll interval before it is
// enqueued. That is the price of not lying about atomicity.
package relay

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/singha105/webhook-relay/internal/queue"
	"github.com/singha105/webhook-relay/internal/store"
	"github.com/singha105/webhook-relay/internal/telemetry"
)

// Defaults.
const (
	// DefaultPollInterval is how often the relay looks for due work. It bounds
	// the added latency for a newly ingested event, so it is deliberately
	// short; the query it runs is a single indexed scan.
	DefaultPollInterval = 250 * time.Millisecond
	// DefaultBatchSize bounds one claim.
	DefaultBatchSize = 100
	// DefaultLease is how long a claimed event may sit in 'delivering' before
	// it is presumed abandoned. It must comfortably exceed one delivery
	// timeout plus the stale-reclaim interval, or a slow-but-healthy delivery
	// would be requeued underneath the worker still performing it.
	DefaultLease = 5 * time.Minute
	// DefaultSweepInterval is how often expired leases are requeued. This is a
	// backstop, not the primary recovery path, so it runs far less often than
	// the poll.
	DefaultSweepInterval = 30 * time.Second
)

// Config configures the relay.
type Config struct {
	PollInterval  time.Duration
	BatchSize     int
	Lease         time.Duration
	SweepInterval time.Duration
}

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.Lease <= 0 {
		c.Lease = DefaultLease
	}
	if c.SweepInterval <= 0 {
		c.SweepInterval = DefaultSweepInterval
	}
	return c
}

// Relay moves due events from Postgres into the queue.
type Relay struct {
	store  *store.Store
	queue  queue.Queue
	logger *slog.Logger
	cfg    Config
}

// New builds a Relay.
func New(st *store.Store, q queue.Queue, logger *slog.Logger, cfg Config) *Relay {
	return &Relay{store: st, queue: q, logger: logger, cfg: cfg.withDefaults()}
}

// Run polls until ctx is cancelled.
//
// Safe to run in several replicas at once: ClaimDueEvents uses FOR UPDATE SKIP
// LOCKED, so concurrent relays take disjoint batches rather than contending or
// double-enqueueing.
func (r *Relay) Run(ctx context.Context) error {
	poll := time.NewTicker(r.cfg.PollInterval)
	defer poll.Stop()
	sweep := time.NewTicker(r.cfg.SweepInterval)
	defer sweep.Stop()

	r.logger.Info("outbox relay started",
		slog.Duration("poll_interval", r.cfg.PollInterval),
		slog.Int("batch_size", r.cfg.BatchSize),
		slog.Duration("lease", r.cfg.Lease),
	)

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("outbox relay stopped")
			return nil

		case <-poll.C:
			// Drain: keep going while full batches come back, so a backlog is
			// cleared at the speed of the database rather than one batch per
			// tick. Bounded so a permanent backlog cannot starve the sweep.
			for i := 0; i < 10; i++ {
				n, err := r.relayOnce(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					r.logger.Error("relay poll failed", slog.Any("error", err))
					break
				}
				if n < r.cfg.BatchSize {
					break
				}
			}

		case <-sweep.C:
			r.sweepExpiredLeases(ctx)
		}
	}
}

// relayOnce claims one batch and enqueues it, returning how many were claimed.
func (r *Relay) relayOnce(ctx context.Context) (int, error) {
	due, err := r.store.ClaimDueEvents(ctx, r.cfg.BatchSize, r.cfg.Lease)
	if err != nil {
		return 0, err
	}
	if len(due) == 0 {
		return 0, nil
	}

	enqueued := 0
	for _, ev := range due {
		id := ev.ID

		// Rehydrate the trace context that was captured at ingest. The relay
		// has no in-process link to that request — it is a background poller
		// reading rows written minutes ago — so this is the only way the
		// delivery ends up in the same trace as the ingest that created it.
		//
		// A span here would be noise (the relay is a poll loop, not a unit of
		// work), so the context is passed through for Enqueue to inject into
		// the message and nothing more.
		enqueueCtx := telemetry.ExtractContext(ctx, ev.TraceContext)

		if err := r.queue.Enqueue(enqueueCtx, id); err != nil {
			if ctx.Err() != nil {
				// Shutting down. The remaining events keep their lease and
				// will be requeued when it expires; nothing is lost. The
				// enqueue error is a symptom of cancellation, not a fault to
				// report.
				//nolint:nilerr // deliberate: a cancelled shutdown is not an error
				return enqueued, nil
			}
			// The enqueue failed, so this event has a lease and no message.
			// Release it immediately rather than making it wait out the full
			// lease — Valkey being briefly unavailable is the common case here.
			r.logger.Error("enqueue failed, releasing claim",
				slog.String("event_id", id.String()),
				slog.Any("error", err),
			)
			if relErr := r.store.ReleaseClaim(ctx, id); relErr != nil {
				// Now the event will wait out its lease. Still correct, just
				// slower, so this is a warning rather than an error.
				r.logger.Warn("could not release claim; the lease will expire instead",
					slog.String("event_id", id.String()),
					slog.Any("error", relErr),
				)
			}
			continue
		}
		enqueued++
	}

	r.logger.Debug("relayed a batch",
		slog.Int("claimed", len(due)),
		slog.Int("enqueued", enqueued),
	)
	return len(due), nil
}

// sweepExpiredLeases returns abandoned events to the ready set.
func (r *Relay) sweepExpiredLeases(ctx context.Context) {
	n, err := r.store.RequeueExpiredLeases(ctx, r.cfg.BatchSize)
	if err != nil {
		if ctx.Err() == nil {
			r.logger.Error("lease sweep failed", slog.Any("error", err))
		}
		return
	}
	if n > 0 {
		// Worth a warning, not a debug line: this only fires when a worker died
		// holding an event AND the stream could not recover it. It is a real
		// signal that something upstream is unhealthy.
		r.logger.Warn("requeued events whose delivery lease expired",
			slog.Int("count", n),
			slog.String("meaning", "a worker died holding these and the stream did not recover them"),
		)
	}
}

// EnqueueNow pushes a single event immediately, bypassing the poll interval.
//
// Used by the replay endpoint so a manually replayed event does not appear to
// hang for a poll interval while an operator watches. It is an optimization
// only: if it fails, the ordinary relay loop still picks the event up, so the
// caller can safely ignore the error.
func (r *Relay) EnqueueNow(ctx context.Context, eventID uuid.UUID) error {
	if err := r.queue.Enqueue(ctx, eventID); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}
