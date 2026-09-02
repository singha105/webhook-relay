// Package worker consumes delivery work and performs it.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/singha105/webhook-relay/internal/breaker"
	"github.com/singha105/webhook-relay/internal/delivery"
	"github.com/singha105/webhook-relay/internal/models"
	"github.com/singha105/webhook-relay/internal/queue"
	"github.com/singha105/webhook-relay/internal/ratelimit"
	"github.com/singha105/webhook-relay/internal/store"
	"github.com/singha105/webhook-relay/internal/telemetry"
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
	// DefaultDrainTimeout bounds how long shutdown waits for in-flight
	// deliveries. It must exceed one delivery timeout, or a healthy delivery
	// gets abandoned every time a pod is rolled.
	DefaultDrainTimeout = 30 * time.Second
	// deferredRetryJitter spreads rescheduled events that were deferred at the
	// same instant, so a rate-limited endpoint does not receive its whole
	// backlog in one burst the moment its bucket refills.
	deferredRetryJitter = 500 * time.Millisecond
)

// Config configures the worker pool.
type Config struct {
	Concurrency     int
	PollInterval    time.Duration
	BatchSize       int
	StaleTimeout    time.Duration
	ReclaimInterval time.Duration
	Backoff         delivery.Backoff
	// DrainTimeout bounds the shutdown drain.
	DrainTimeout time.Duration
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
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
	return c
}

// Pool is a set of goroutines consuming delivery work.
type Pool struct {
	store   *store.Store
	queue   queue.Queue
	client  *delivery.Client
	deduper *delivery.Deduper
	limiter *ratelimit.Limiter
	breaker *breaker.Breaker
	metrics *telemetry.Metrics
	tracer  trace.Tracer
	logger  *slog.Logger
	cfg     Config
	// name distinguishes this process's consumers in the group. Each goroutine
	// appends its index, so XPENDING shows which goroutine is holding what.
	name string

	// inFlight counts deliveries currently being performed. Shutdown waits on
	// it so a bounded drain can tell "still working" from "idle".
	inFlight atomic.Int64
	// draining stops the consume loops claiming new work while letting the
	// deliveries they already hold finish.
	draining atomic.Bool
}

// Deps bundles the collaborators a Pool needs, so adding one does not keep
// growing a positional constructor that call sites get subtly wrong.
type Deps struct {
	Store   *store.Store
	Queue   queue.Queue
	Client  *delivery.Client
	Deduper *delivery.Deduper
	Limiter *ratelimit.Limiter
	Breaker *breaker.Breaker
	Metrics *telemetry.Metrics
	Tracer  trace.Tracer
	Logger  *slog.Logger
	// Name identifies this process in the consumer group.
	Name string
}

// New builds a Pool.
func New(deps Deps, cfg Config) *Pool {
	tracer := deps.Tracer
	if tracer == nil {
		tracer = noop.NewTracerProvider().Tracer("")
	}
	return &Pool{
		store:   deps.Store,
		queue:   deps.Queue,
		client:  deps.Client,
		deduper: deps.Deduper,
		limiter: deps.Limiter,
		breaker: deps.Breaker,
		metrics: deps.Metrics,
		tracer:  tracer,
		logger:  deps.Logger,
		cfg:     cfg.withDefaults(),
		name:    deps.Name,
	}
}

// InFlight reports how many deliveries are currently in progress.
func (p *Pool) InFlight() int64 { return p.inFlight.Load() }

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

// Drain stops the pool claiming new work and waits, up to DrainTimeout, for the
// deliveries already in flight to finish.
//
// This is the SIGTERM path, and the ordering is what makes it safe:
//
//  1. Set draining. The consume loops finish their current message and then
//     stop claiming. New work stays in the stream, untouched, for another
//     replica or for this one after it restarts.
//  2. Wait for in-flight deliveries. Each is already bounded by the delivery
//     timeout, so this terminates. Every one that completes is acked, so it is
//     not redelivered.
//  3. If the budget runs out, return. Whatever is still in flight was never
//     acked, so it stays in the pending list and is reclaimed by another worker
//     after the stale timeout — delivered again rather than lost.
//
// Nothing is lost in any of those paths. The worst case is a duplicate, which
// is the at-least-once contract the receiver already has to tolerate.
func (p *Pool) Drain(ctx context.Context) error {
	p.draining.Store(true)

	deadline := time.NewTimer(p.cfg.DrainTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	start := time.Now()
	for {
		if n := p.inFlight.Load(); n == 0 {
			p.logger.Info("drain complete; no deliveries were in flight",
				slog.Duration("took", time.Since(start).Round(time.Millisecond)))
			return nil
		}
		select {
		case <-deadline.C:
			n := p.inFlight.Load()
			// Deliberately not an error return: this is a bounded drain doing
			// exactly what it promised. It is a warning because those messages
			// will be redelivered, which is worth knowing about.
			p.logger.Warn("drain budget exhausted; in-flight deliveries will be reclaimed and retried",
				slog.Int64("still_in_flight", n),
				slog.Duration("budget", p.cfg.DrainTimeout),
			)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// consume is one delivery goroutine.
func (p *Pool) consume(ctx context.Context, consumerID string) {
	for {
		if ctx.Err() != nil {
			return
		}
		// Stop taking new work as soon as shutdown begins. Anything left in
		// the stream is untouched and will be picked up by another replica or
		// by this one on restart.
		if p.draining.Load() {
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
			// Deliberately NOT checking ctx or draining here: these messages
			// are already claimed, so abandoning them would leave them in the
			// pending list to be reclaimed and delivered again. Finishing them
			// is both faster and safer, and Drain waits for exactly this.
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
func (p *Pool) processMessage(parentCtx context.Context, consumerID string, msg queue.ClaimedMessage) {
	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)

	// Join the trace that started at ingest. Without this the delivery would
	// open its own unconnected trace and the queue hop would be invisible.
	ctx := telemetry.ExtractContext(parentCtx, msg.TraceFields)
	ctx, span := p.tracer.Start(ctx, "webhook.deliver",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			telemetry.AttrEventID(msg.EventID.String()),
			telemetry.AttrMessageID(msg.MessageID),
			attribute.Int64("messaging.delivery_count", msg.DeliveryCount),
		),
	)
	defer span.End()

	log := p.logger.With(
		slog.String("event_id", msg.EventID.String()),
		slog.String("consumer", consumerID),
		slog.String("message_id", msg.MessageID),
	)
	// Stamp the trace id on every log line for this delivery, so a line found
	// in Loki pivots straight to its trace in Tempo.
	if traceID := telemetry.TraceIDFrom(ctx); traceID != "" {
		log = log.With(slog.String("trace_id", traceID))
	}

	target, err := p.store.LoadForDelivery(ctx, msg.EventID)
	if err != nil {
		if errors.Is(err, models.ErrNotFound) {
			// The event or its endpoint was deleted after the message was
			// enqueued. There is nothing to deliver and nothing to retry, so
			// ack it rather than leaving it to be reclaimed forever.
			log.Info("event no longer exists; discarding the message")
			span.SetStatus(codes.Ok, "event no longer exists")
			p.ack(ctx, log, msg.MessageID)
			return
		}
		// A transient database problem. Do NOT ack: leaving the entry pending
		// means the reclaim sweep will retry it once Postgres is back.
		log.Error("could not load event for delivery", slog.Any("error", err))
		span.RecordError(err)
		span.SetStatus(codes.Error, "load failed")
		return
	}

	span.SetAttributes(
		telemetry.AttrEndpointID(target.Endpoint.ID.String()),
		telemetry.AttrEventType(target.Event.EventType),
	)
	log = log.With(slog.String("endpoint_id", target.Endpoint.ID.String()))

	// An endpoint can be deactivated while its events are in flight. Park the
	// event rather than delivering to somewhere the customer has switched off.
	if !target.Endpoint.IsActive {
		log.Info("endpoint is inactive; dead-lettering without an attempt")
		p.deadLetterWithoutAttempt(ctx, log, target)
		span.SetStatus(codes.Ok, "endpoint inactive")
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

	// The circuit breaker comes BEFORE the rate limiter. An open breaker means
	// we should not be calling this endpoint at all, so spending one of its
	// rate-limit tokens to discover that would throttle the endpoint's eventual
	// recovery for no reason.
	if !p.checkBreaker(ctx, log, span, target, msg) {
		return
	}
	if !p.checkRateLimit(ctx, log, span, target, msg) {
		return
	}

	attempt, err := p.store.NextAttemptNumber(ctx, msg.EventID)
	if err != nil {
		log.Error("could not determine the attempt number", slog.Any("error", err))
		span.RecordError(err)
		return
	}
	span.SetAttributes(telemetry.AttrAttempt(attempt))

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
		span.SetStatus(codes.Ok, "duplicate suppressed")
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
		span.RecordError(err)
		span.SetStatus(codes.Error, "request build failed")
		if relErr := p.deduper.Release(ctx, msg.EventID, attempt); relErr != nil {
			log.Warn("could not release the dedup marker", slog.Any("error", relErr))
		}
		return
	}

	p.recordMetrics(ctx, target, result)
	p.annotateSpan(span, result)
	p.recordOutcome(ctx, log, target, attempt, result)
	p.ack(ctx, log, msg.MessageID)
}

// checkBreaker consults the circuit breaker. Returns false when the delivery
// must not proceed, having already rescheduled and acked.
func (p *Pool) checkBreaker(ctx context.Context, log *slog.Logger, span trace.Span, target *store.DeliveryTarget, msg queue.ClaimedMessage) bool {
	decision, err := p.breaker.Allow(ctx, target.Endpoint.ID)
	if err != nil {
		// Fails open, so decision.Allowed is true. Visible, not silent.
		log.Warn("circuit breaker unavailable, proceeding", slog.Any("error", err))
	}
	span.SetAttributes(attribute.String("webhook.breaker_state", string(decision.State)))
	if decision.Allowed {
		return true
	}

	// Not an attempt: the receiver never saw a request. Deferring without
	// burning the budget is the whole point — otherwise an endpoint that is
	// down long enough would have every event dead-lettered by the breaker
	// that exists to protect it.
	delay := decision.RetryAfter
	if delay <= 0 {
		delay = time.Second
	}
	log.Info("circuit breaker is open; deferring without an attempt",
		slog.String("state", string(decision.State)),
		slog.Duration("retry_after", delay.Round(time.Millisecond)),
	)
	span.SetStatus(codes.Ok, "deferred by circuit breaker")
	p.deferDelivery(ctx, log, msg, target.Event.ID, delay)
	return false
}

// checkRateLimit consults the token bucket. Returns false when the delivery
// must not proceed, having already rescheduled and acked.
func (p *Pool) checkRateLimit(ctx context.Context, log *slog.Logger, span trace.Span, target *store.DeliveryTarget, msg queue.ClaimedMessage) bool {
	decision, err := p.limiter.Allow(ctx, target.Endpoint.ID, target.Endpoint.RateLimitPerSec)
	if err != nil {
		log.Warn("rate limiter unavailable, proceeding", slog.Any("error", err))
	}
	if decision.Allowed {
		return true
	}

	if p.metrics != nil {
		p.metrics.RecordRateLimited(ctx, target.Endpoint.ID.String())
	}

	// A slow consumer is not a failed delivery. No attempt row, no counter
	// increment — just come back when there is capacity.
	delay := decision.RetryAfter
	if delay <= 0 {
		delay = 250 * time.Millisecond
	}
	log.Debug("rate limited; deferring without an attempt",
		slog.Int("rate_limit_per_sec", target.Endpoint.RateLimitPerSec),
		slog.Duration("retry_after", delay.Round(time.Millisecond)),
	)
	span.SetAttributes(attribute.Bool("webhook.rate_limited", true))
	span.SetStatus(codes.Ok, "deferred by rate limit")
	p.deferDelivery(ctx, log, msg, target.Event.ID, delay)
	return false
}

// deferDelivery reschedules an event without consuming an attempt, then acks
// the message so the entry does not sit in the pending list until it is
// reclaimed.
func (p *Pool) deferDelivery(ctx context.Context, log *slog.Logger, msg queue.ClaimedMessage, eventID uuid.UUID, delay time.Duration) {
	// Jitter, for the same reason retries are jittered: everything deferred at
	// the same instant would otherwise come back at the same instant and
	// re-saturate the limit the moment it lifts.
	delay += time.Duration(rand.Int64N(int64(deferredRetryJitter)))

	if err := p.store.RescheduleWithoutAttempt(ctx, eventID, time.Now().Add(delay)); err != nil {
		// Not acking here would be worse: the entry would be reclaimed later
		// and re-evaluated, which is the same outcome with extra latency.
		log.Error("could not reschedule a deferred delivery", slog.Any("error", err))
	}
	p.ack(ctx, log, msg.MessageID)
}

// recordMetrics emits the counters and histogram for one completed attempt.
func (p *Pool) recordMetrics(ctx context.Context, target *store.DeliveryTarget, result delivery.Result) {
	if p.metrics == nil {
		return
	}
	p.metrics.RecordDeliveryAttempt(ctx,
		statusClass(result),
		target.Endpoint.ID.String(),
		result.Duration.Seconds(),
	)
}

// annotateSpan records the outcome on the delivery span.
func (p *Pool) annotateSpan(span trace.Span, result delivery.Result) {
	span.SetAttributes(telemetry.AttrOutcome(result.Outcome.String()))
	if result.StatusCode != nil {
		span.SetAttributes(telemetry.AttrStatusCode(*result.StatusCode))
	}
	switch result.Outcome {
	case delivery.OutcomeSuccess:
		span.SetStatus(codes.Ok, "")
	default:
		if result.Err != nil {
			span.RecordError(result.Err)
		}
		span.SetStatus(codes.Error, result.Outcome.String())
	}
}

// statusClass buckets a result for the metric label.
//
// Coarse on purpose: a label per distinct status code multiplies series for no
// analytical gain, because nobody alerts on "503 specifically".
func statusClass(result delivery.Result) string {
	if result.StatusCode == nil {
		// No HTTP response at all — DNS, refused connection, timeout. Kept
		// distinct from 5xx because they mean different things about where the
		// fault is.
		return "error"
	}
	switch code := *result.StatusCode; {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500:
		return "5xx"
	default:
		return "other"
	}
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
		if err := p.breaker.RecordSuccess(ctx, target.Endpoint.ID); err != nil {
			log.Warn("could not close the circuit breaker after a success", slog.Any("error", err))
		}
		log.Info("delivered")

	case delivery.OutcomePermanent:
		// Straight to the DLQ without burning the remaining attempts: no
		// number of retries fixes a 404 or a rejected signature.
		if err := p.store.MarkDeadLettered(ctx, target.Event.ID, target.Endpoint.ID, record); err != nil {
			log.Error("could not dead-letter after a permanent failure", slog.Any("error", err))
			return
		}
		p.recordFailure(ctx, log, target, "permanent")
		log.Warn("permanent failure; dead-lettered without further retries")

	case delivery.OutcomeRetryable:
		if !p.cfg.Backoff.ShouldRetry(attempt) {
			if err := p.store.MarkDeadLettered(ctx, target.Event.ID, target.Endpoint.ID, record); err != nil {
				log.Error("could not dead-letter after exhausting retries", slog.Any("error", err))
				return
			}
			p.recordFailure(ctx, log, target, "exhausted")
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
		p.recordBreakerFailure(ctx, log, target)
		log.Warn("delivery failed; retry scheduled",
			slog.Duration("delay", delay.Round(time.Millisecond)),
			slog.Duration("ceiling", p.cfg.Backoff.Ceiling(attempt)),
			slog.Time("next_attempt_at", next),
			slog.Any("error", result.Err),
		)
	}
}

// recordFailure updates the breaker and counts the dead-letter.
func (p *Pool) recordFailure(ctx context.Context, log *slog.Logger, target *store.DeliveryTarget, reason string) {
	p.recordBreakerFailure(ctx, log, target)
	if p.metrics != nil {
		p.metrics.RecordDLQ(ctx, target.Endpoint.ID.String(), reason)
	}
}

// recordBreakerFailure increments the endpoint's consecutive failure count,
// opening the breaker at the threshold.
func (p *Pool) recordBreakerFailure(ctx context.Context, log *slog.Logger, target *store.DeliveryTarget) {
	state, failures, err := p.breaker.RecordFailure(ctx, target.Endpoint.ID)
	if err != nil {
		log.Warn("could not record a breaker failure", slog.Any("error", err))
		return
	}
	if state == breaker.StateOpen {
		// Loud: the endpoint is now being skipped entirely, which an operator
		// needs to know before they wonder why deliveries stopped.
		log.Warn("circuit breaker opened for this endpoint",
			slog.Int("consecutive_failures", failures),
			slog.String("state", string(state)),
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
