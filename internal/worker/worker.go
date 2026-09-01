// Package worker consumes delivery work and performs it.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/singha105/webhook-relay/internal/delivery"
	"github.com/singha105/webhook-relay/internal/models"
	"github.com/singha105/webhook-relay/internal/queue"
	"github.com/singha105/webhook-relay/internal/store"
)

// Defaults.
const (
	// DefaultConcurrency is the number of delivery goroutines.
	DefaultConcurrency = 10
	// DefaultPollInterval is how long a goroutine waits after finding the
	// queue empty. Claim does not block, so this is what stops an idle worker
	// from spinning.
	DefaultPollInterval = 200 * time.Millisecond
	// DefaultBatchSize is how many entries one goroutine claims at a time.
	// Kept small: a goroutine that claims fifty entries holds all fifty in its
	// own pending list while working through them serially, which delays
	// recovery if it dies.
	DefaultBatchSize = 5
	// DefaultStaleTimeout is how long an entry may be held without an ack
	// before another worker may take it. It must exceed the delivery timeout
	// with margin, or a slow-but-healthy delivery would be reclaimed and
	// duplicated while it is still in flight.
	DefaultStaleTimeout = 60 * time.Second
	// DefaultReclaimInterval is how often the reclaim sweep runs.
	DefaultReclaimInterval = 15 * time.Second
)

// Config configures the worker pool.
type Config struct {
	Concurrency     int
	PollInterval    time.Duration
	BatchSize       int
	StaleTimeout    time.Duration
	ReclaimInterval time.Duration
	Backoff         delivery.Backoff
}

func (c Config) withDefaults() Config {
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.StaleTimeout <= 0 {
		c.StaleTimeout = DefaultStaleTimeout
	}
	if c.ReclaimInterval <= 0 {
		c.ReclaimInterval = DefaultReclaimInterval
	}
	if c.Backoff.MaxAttempts == 0 {
		c.Backoff = delivery.NewBackoff(0, 0, 0)
	}
	return c
}

// Pool is a set of goroutines consuming delivery work.
type Pool struct {
	store   *store.Store
	queue   queue.Queue
	client  *delivery.Client
	deduper *delivery.Deduper
	logger  *slog.Logger
	cfg     Config
	// name distinguishes this process's consumers in the group. Each goroutine
	// appends its index, so XPENDING shows which goroutine is holding what.
	name string
}

// New builds a Pool.
func New(st *store.Store, q queue.Queue, client *delivery.Client, deduper *delivery.Deduper, logger *slog.Logger, name string, cfg Config) *Pool {
	return &Pool{
		store:   st,
		queue:   q,
		client:  client,
		deduper: deduper,
		logger:  logger,
		cfg:     cfg.withDefaults(),
		name:    name,
	}
}

// Run starts the pool and blocks until ctx is cancelled and every goroutine has
// finished the delivery it was performing.
func (p *Pool) Run(ctx context.Context) error {
	p.logger.Info("delivery worker pool started",
		slog.Int("concurrency", p.cfg.Concurrency),
		slog.Int("batch_size", p.cfg.BatchSize),
		slog.Duration("stale_timeout", p.cfg.StaleTimeout),
		slog.Int("max_attempts", p.cfg.Backoff.MaxAttempts),
		slog.Bool("delivery_dedup", p.deduper.Enabled()),
	)
	if !p.deduper.Enabled() {
		// Loud, because this is a deliberate weakening of the delivery
		// guarantee and it must never be on by accident in a real deployment.
		p.logger.Warn("delivery deduplication is DISABLED; a reclaimed message can be delivered twice")
	}

	var wg sync.WaitGroup
	for i := 0; i < p.cfg.Concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			p.consume(ctx, fmt.Sprintf("%s-%d", p.name, idx))
		}(i)
	}

	// One reclaim sweeper for the whole process, not one per goroutine:
	// XAUTOCLAIM walks the pending list, and running it from ten goroutines
	// would multiply that scan for no benefit.
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.reclaimLoop(ctx, p.name+"-reclaimer")
	}()

	wg.Wait()
	p.logger.Info("delivery worker pool stopped")
	return nil
}

// consume is one delivery goroutine.
func (p *Pool) consume(ctx context.Context, consumerID string) {
	for {
		if ctx.Err() != nil {
			return
		}

		msgs, err := p.queue.Claim(ctx, consumerID, p.cfg.BatchSize)
		switch {
		case errors.Is(err, queue.ErrEmpty):
			if !sleepCtx(ctx, p.cfg.PollInterval) {
				return
			}
			continue
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			p.logger.Error("claim failed", slog.String("consumer", consumerID), slog.Any("error", err))
			if !sleepCtx(ctx, p.cfg.PollInterval) {
				return
			}
			continue
		}

		for _, msg := range msgs {
			// Deliberately NOT checking ctx here: a claimed message is being
			// processed, and abandoning it mid-flight would leave it in the
			// pending list to be reclaimed later. Finishing the delivery is
			// both faster and safer. Shutdown waits for this.
			p.processMessage(ctx, consumerID, msg)
		}
	}
}

// reclaimLoop recovers entries from workers that died holding them.
func (p *Pool) reclaimLoop(ctx context.Context, consumerID string) {
	ticker := time.NewTicker(p.cfg.ReclaimInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			msgs, err := p.queue.ReclaimStale(ctx, consumerID, p.cfg.StaleTimeout, p.cfg.BatchSize*p.cfg.Concurrency)
			if errors.Is(err, queue.ErrEmpty) {
				continue
			}
			if err != nil {
				if ctx.Err() == nil {
					p.logger.Error("stale reclaim failed", slog.Any("error", err))
				}
				continue
			}

			p.logger.Warn("reclaimed messages from a consumer that stopped responding",
				slog.Int("count", len(msgs)),
				slog.Duration("idle_for", p.cfg.StaleTimeout),
			)
			for _, msg := range msgs {
				p.processMessage(ctx, consumerID, msg)
			}
		}
	}
}

// processMessage performs one delivery attempt end to end.
func (p *Pool) processMessage(ctx context.Context, consumerID string, msg queue.ClaimedMessage) {
	log := p.logger.With(
		slog.String("event_id", msg.EventID.String()),
		slog.String("consumer", consumerID),
		slog.String("message_id", msg.MessageID),
	)

	target, err := p.store.LoadForDelivery(ctx, msg.EventID)
	if err != nil {
		if errors.Is(err, models.ErrNotFound) {
			// The event or its endpoint was deleted after the message was
			// enqueued. There is nothing to deliver and nothing to retry, so
			// ack it rather than leaving it to be reclaimed forever.
			log.Info("event no longer exists; discarding the message")
			p.ack(ctx, log, msg.MessageID)
			return
		}
		// A transient database problem. Do NOT ack: leaving the entry pending
		// means the reclaim sweep will retry it once Postgres is back.
		log.Error("could not load event for delivery", slog.Any("error", err))
		return
	}

	// An endpoint can be deactivated while its events are in flight. Park the
	// event rather than delivering to somewhere the customer has switched off.
	if !target.Endpoint.IsActive {
		log.Info("endpoint is inactive; dead-lettering without an attempt")
		p.deadLetterWithoutAttempt(ctx, log, target)
		p.ack(ctx, log, msg.MessageID)
		return
	}

	// Terminal already — a duplicate message for an event another worker
	// finished. Ack and move on.
	if target.Event.Status == models.StatusDelivered || target.Event.Status == models.StatusDLQ {
		log.Debug("event is already in a terminal state; discarding the message",
			slog.String("status", string(target.Event.Status)))
		p.ack(ctx, log, msg.MessageID)
		return
	}

	attempt, err := p.store.NextAttemptNumber(ctx, msg.EventID)
	if err != nil {
		log.Error("could not determine the attempt number", slog.Any("error", err))
		return
	}

	// The dedup guard. If another worker already dispatched this exact
	// (event, attempt), do not send it again.
	mayDispatch, dedupErr := p.deduper.Claim(ctx, msg.EventID, attempt)
	if dedupErr != nil {
		// Claim fails open, so mayDispatch is true here. Log and continue: a
		// possible duplicate is better than a stalled delivery.
		log.Warn("delivery dedup unavailable, proceeding", slog.Any("error", dedupErr))
	}
	if !mayDispatch {
		log.Warn("suppressed a duplicate dispatch",
			slog.Int("attempt", attempt),
			slog.Int64("delivery_count", msg.DeliveryCount),
		)
		p.ack(ctx, log, msg.MessageID)
		return
	}

	result, err := p.client.Deliver(ctx, delivery.Request{
		Endpoint: &target.Endpoint,
		Event:    &target.Event,
		Attempt:  attempt,
	})
	if err != nil {
		// The request could not even be built — a malformed URL that passed
		// validation, for instance. Release the dedup marker so the retry is
		// not blocked by it.
		log.Error("could not build the delivery request", slog.Any("error", err))
		if relErr := p.deduper.Release(ctx, msg.EventID, attempt); relErr != nil {
			log.Warn("could not release the dedup marker", slog.Any("error", relErr))
		}
		return
	}

	p.recordOutcome(ctx, log, target, attempt, result)
	p.ack(ctx, log, msg.MessageID)
}

// recordOutcome persists the attempt and moves the event to its next state.
func (p *Pool) recordOutcome(ctx context.Context, log *slog.Logger, target *store.DeliveryTarget, attempt int, result delivery.Result) {
	record := models.DeliveryAttempt{
		EventID:       target.Event.ID,
		AttemptNumber: attempt,
		StatusCode:    result.StatusCode,
		ResponseBody:  result.ResponseBody,
		DurationMS:    int(result.Duration.Milliseconds()),
	}
	if result.ErrorMessage != "" {
		msg := result.ErrorMessage
		record.ErrorMessage = &msg
	}

	log = log.With(
		slog.Int("attempt", attempt),
		slog.String("outcome", result.Outcome.String()),
		slog.Int64("duration_ms", result.Duration.Milliseconds()),
	)
	if result.StatusCode != nil {
		log = log.With(slog.Int("status", *result.StatusCode))
	}

	switch result.Outcome {
	case delivery.OutcomeSuccess:
		if err := p.store.MarkDelivered(ctx, target.Event.ID, target.Endpoint.ID, record); err != nil {
			log.Error("delivered, but could not record it", slog.Any("error", err))
			return
		}
		log.Info("delivered")

	case delivery.OutcomePermanent:
		// Straight to the DLQ without burning the remaining attempts: no
		// number of retries fixes a 404 or a rejected signature.
		if err := p.store.MarkDeadLettered(ctx, target.Event.ID, target.Endpoint.ID, record); err != nil {
			log.Error("could not dead-letter after a permanent failure", slog.Any("error", err))
			return
		}
		log.Warn("permanent failure; dead-lettered without further retries")

	case delivery.OutcomeRetryable:
		if !p.cfg.Backoff.ShouldRetry(attempt) {
			if err := p.store.MarkDeadLettered(ctx, target.Event.ID, target.Endpoint.ID, record); err != nil {
				log.Error("could not dead-letter after exhausting retries", slog.Any("error", err))
				return
			}
			log.Warn("retries exhausted; dead-lettered",
				slog.Int("max_attempts", p.cfg.Backoff.MaxAttempts))
			return
		}

		delay := p.cfg.Backoff.Delay(attempt)
		// A server-supplied Retry-After wins when it asks for longer than our
		// own backoff. Taking the max rather than replacing outright means a
		// receiver cannot shorten our backoff by sending "Retry-After: 0".
		if result.RetryAfter > delay {
			delay = result.RetryAfter
		}
		next := time.Now().Add(delay)

		if err := p.store.ScheduleRetry(ctx, target.Event.ID, target.Endpoint.ID, record, next); err != nil {
			log.Error("could not schedule a retry", slog.Any("error", err))
			return
		}
		log.Warn("delivery failed; retry scheduled",
			slog.Duration("delay", delay.Round(time.Millisecond)),
			slog.Duration("ceiling", p.cfg.Backoff.Ceiling(attempt)),
			slog.Time("next_attempt_at", next),
			slog.Any("error", result.Err),
		)
	}
}

// deadLetterWithoutAttempt parks an event for an inactive endpoint. It records
// a synthetic attempt so the reason is visible in the event's history rather
// than only in a log line that will have rotated away.
func (p *Pool) deadLetterWithoutAttempt(ctx context.Context, log *slog.Logger, target *store.DeliveryTarget) {
	attempt, err := p.store.NextAttemptNumber(ctx, target.Event.ID)
	if err != nil {
		attempt = target.Event.AttemptCount + 1
	}
	msg := "endpoint is inactive; delivery was not attempted"
	record := models.DeliveryAttempt{
		EventID:       target.Event.ID,
		AttemptNumber: attempt,
		ErrorMessage:  &msg,
	}
	if err := p.store.MarkDeadLettered(ctx, target.Event.ID, target.Endpoint.ID, record); err != nil {
		log.Error("could not dead-letter for an inactive endpoint", slog.Any("error", err))
	}
}

func (p *Pool) ack(ctx context.Context, log *slog.Logger, messageID string) {
	// Acking must not be skipped because the parent context was cancelled
	// during shutdown: an unacked entry sits in the pending list until the
	// reclaim timeout, which would redeliver work that is already done.
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := p.queue.Ack(ackCtx, messageID); err != nil {
		log.Error("ack failed; the entry will be reclaimed and redelivered", slog.Any("error", err))
	}
}

// sleepCtx sleeps unless the context is cancelled first. Returns false if it
// was cancelled, so callers can return promptly.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
