package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/singha105/webhook-relay/internal/telemetry"
)

// Stream and group names. Valkey is protocol-compatible with Redis, so the
// go-redis client is used unchanged; nothing here depends on a Redis-only
// command.
const (
	// StreamKey holds delivery work.
	StreamKey = "webhook-relay:deliveries"
	// GroupName is the single consumer group. Every worker joins it, so each
	// entry goes to exactly one worker rather than being fanned out.
	GroupName = "delivery-workers"
	// fieldEventID identifies the event to deliver.
	fieldEventID = "event_id"
)

// traceFieldPrefix namespaces propagated trace context inside a stream entry.
//
// Prefixed so trace plumbing can never collide with a future payload field,
// and so a human reading XRANGE output can tell instantly which fields are
// ours and which belong to OpenTelemetry.
const traceFieldPrefix = "otel_"

// ValkeyQueue is a Queue backed by a Valkey stream with one consumer group.
type ValkeyQueue struct {
	client  *redis.Client
	maxLen  int64
	stream  string
	group   string
	nowFunc func() time.Time
}

// ValkeyConfig configures the queue.
type ValkeyConfig struct {
	// URL is a redis:// connection string.
	URL string
	// MaxLen bounds the stream with XADD MAXLEN ~. Without a bound the stream
	// grows forever: XACK removes an entry from the pending list but not from
	// the stream itself, so acknowledged history accumulates until something
	// trims it. The ~ makes trimming approximate, which lets Valkey trim whole
	// nodes instead of walking entries — materially cheaper, at the cost of the
	// stream sometimes sitting slightly above the bound.
	MaxLen int64
	// PoolSize bounds connections. Defaults to the worker count when zero.
	PoolSize int

	// Stream and Group override the default key names. Production leaves both
	// empty; integration tests set them so each test owns an isolated stream
	// in a shared container, which scales further than the 16 numbered logical
	// databases and does not race the way FLUSHDB between tests would.
	Stream string
	Group  string
}

// NewValkeyQueue connects, verifies reachability, and creates the consumer
// group if it does not exist.
func NewValkeyQueue(ctx context.Context, cfg ValkeyConfig) (*ValkeyQueue, error) {
	opt, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse valkey url: %w", err)
	}
	if cfg.PoolSize > 0 {
		opt.PoolSize = cfg.PoolSize
	}
	client := redis.NewClient(opt)

	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping valkey: %w", err)
	}

	stream := cfg.Stream
	if stream == "" {
		stream = StreamKey
	}
	group := cfg.Group
	if group == "" {
		group = GroupName
	}

	q := &ValkeyQueue{
		client:  client,
		maxLen:  cfg.MaxLen,
		stream:  stream,
		group:   group,
		nowFunc: time.Now,
	}
	if err := q.ensureGroup(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return q, nil
}

// errNoGroup is the error Valkey returns when the stream or the consumer group
// does not exist.
//
// It is matched by substring because go-redis surfaces it as a plain error
// string rather than a typed value. Fragile in principle; the alternative —
// treating every claim error as possibly-NOGROUP and blindly recreating the
// group — would paper over real failures.
const errNoGroup = "NOGROUP"

// recoverGroup recreates the consumer group after it has vanished.
//
// This is not hypothetical. A Valkey restart without persistence, a flushed
// database, or an operator running FLUSHALL all destroy the stream and its
// group. Without this the workers wedge permanently: XREADGROUP returns NOGROUP
// forever, delivery stops silently, and the only symptom is a log line — while
// the relay happily keeps enqueueing, because XADD recreates the stream but not
// the group.
//
// Found the hard way: flushing Valkey mid-demo stopped delivery entirely and
// the workers never recovered on their own.
func (q *ValkeyQueue) recoverGroup(ctx context.Context, cause error) bool {
	if cause == nil || !strings.Contains(cause.Error(), errNoGroup) {
		return false
	}
	// MKSTREAM from "0" — see ensureGroup for why the start id matters.
	return q.ensureGroup(ctx) == nil
}

// ensureGroup creates the consumer group, tolerating the case where another
// replica created it first.
//
// MKSTREAM creates the stream as a side effect, so a cold start does not have
// to enqueue something before workers can join. Starting at "0" rather than "$"
// is deliberate: "$" would mean "only entries added after this moment", which
// silently abandons anything already sitting in the stream when the group is
// (re)created — exactly the wrong behaviour after a Valkey restart.
func (q *ValkeyQueue) ensureGroup(ctx context.Context) error {
	err := q.client.XGroupCreateMkStream(ctx, q.stream, q.group, "0").Err()
	if err == nil {
		return nil
	}
	// BUSYGROUP means it already exists, which is the normal case for every
	// replica after the first.
	if strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return fmt.Errorf("create consumer group: %w", err)
}

// Enqueue appends an event to the stream, carrying the caller's trace context.
//
// The trace context rides as ordinary string fields. That is the whole reason
// an event's trace can span ingest and delivery: the consumer extracts these
// and starts its span as a child of the producer's, rather than opening an
// unrelated trace that nothing links back.
func (q *ValkeyQueue) Enqueue(ctx context.Context, eventID uuid.UUID) error {
	values := map[string]any{fieldEventID: eventID.String()}
	for k, v := range telemetry.InjectContext(ctx) {
		values[traceFieldPrefix+k] = v
	}

	args := &redis.XAddArgs{
		Stream: q.stream,
		Values: values,
	}
	if q.maxLen > 0 {
		args.MaxLen = q.maxLen
		args.Approx = true
	}
	if err := q.client.XAdd(ctx, args).Err(); err != nil {
		return fmt.Errorf("enqueue event %s: %w", eventID, err)
	}
	return nil
}

// Claim reads new entries for this consumer.
//
// Block is 0 (return immediately) rather than a blocking read. A blocking
// XREADGROUP holds a connection for the whole block and does not observe
// context cancellation until it returns, which makes shutdown ragged. The
// worker owns its own poll interval instead.
func (q *ValkeyQueue) Claim(ctx context.Context, consumerID string, count int) ([]ClaimedMessage, error) {
	read := func() ([]redis.XStream, error) {
		return q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    q.group,
			Consumer: consumerID,
			Streams:  []string{q.stream, ">"}, // ">" = entries never delivered to anyone
			Count:    int64(count),
			Block:    -1, // no blocking
		}).Result()
	}

	streams, err := read()
	if err != nil && q.recoverGroup(ctx, err) {
		// The group was recreated; retry once. A second failure is real.
		streams, err = read()
	}

	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrEmpty
		}
		return nil, fmt.Errorf("claim: %w", err)
	}

	out := make([]ClaimedMessage, 0, count)
	for _, s := range streams {
		for _, msg := range s.Messages {
			cm, convErr := toClaimedMessage(msg, 1)
			if convErr != nil {
				// A malformed entry can never succeed. Ack it so it does not
				// occupy the pending list forever, and keep going.
				_ = q.Ack(ctx, msg.ID)
				continue
			}
			out = append(out, cm)
		}
	}
	if len(out) == 0 {
		return nil, ErrEmpty
	}
	return out, nil
}

// Ack removes an entry from the pending list.
func (q *ValkeyQueue) Ack(ctx context.Context, messageID string) error {
	if err := q.client.XAck(ctx, q.stream, q.group, messageID).Err(); err != nil {
		return fmt.Errorf("ack %s: %w", messageID, err)
	}
	return nil
}

// Nack makes an entry immediately reclaimable by another consumer.
//
// Streams have no "return this to the queue" command and no delayed
// redelivery. What we can do is hand the entry back with its idle timer reset
// to zero, so the next ReclaimStale pass picks it up. retryAfter is therefore
// advisory and is deliberately NOT honoured as a delay — a caller that needs a
// real delay records it in events.next_retry_at and calls Ack, letting the
// outbox relay re-enqueue when it comes due. This method exists for the case
// where a worker knows immediately that it cannot proceed.
func (q *ValkeyQueue) Nack(ctx context.Context, messageID string, retryAfter time.Duration) error {
	// XCLAIM with a nonexistent consumer name and IDLE 0 resets the entry's
	// idle timer and parks it under a consumer that will never poll, so the
	// next XAUTOCLAIM sweep is what picks it up.
	err := q.client.XClaim(ctx, &redis.XClaimArgs{
		Stream:   q.stream,
		Group:    q.group,
		Consumer: nackParkingConsumer,
		MinIdle:  0,
		Messages: []string{messageID},
	}).Err()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("nack %s: %w", messageID, err)
	}
	return nil
}

// nackParkingConsumer owns nacked entries until a reclaim sweep collects them.
// It is never used as a polling consumer, so anything assigned to it is
// guaranteed to age into the reclaim path.
const nackParkingConsumer = "nacked"

// ReclaimStale reassigns entries that have been held without an ack for longer
// than idleTimeout.
//
// This is the recovery path for a worker killed mid-delivery. XAUTOCLAIM walks
// the pending list, so it is the mechanism a plain LIST cannot offer.
func (q *ValkeyQueue) ReclaimStale(ctx context.Context, consumerID string, idleTimeout time.Duration, count int) ([]ClaimedMessage, error) {
	autoClaim := func() ([]redis.XMessage, error) {
		msgs, _, err := q.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   q.stream,
			Group:    q.group,
			Consumer: consumerID,
			MinIdle:  idleTimeout,
			Start:    "0-0",
			Count:    int64(count),
		}).Result()
		return msgs, err
	}

	msgs, err := autoClaim()
	if err != nil && q.recoverGroup(ctx, err) {
		msgs, err = autoClaim()
	}
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrEmpty
		}
		return nil, fmt.Errorf("reclaim stale: %w", err)
	}

	out := make([]ClaimedMessage, 0, len(msgs))
	for _, msg := range msgs {
		cm, convErr := toClaimedMessage(msg, 0)
		if convErr != nil {
			_ = q.Ack(ctx, msg.ID)
			continue
		}
		// Fill in the real delivery count, which is what makes a reclaimed
		// message distinguishable from a first delivery.
		if n, cntErr := q.deliveryCount(ctx, msg.ID); cntErr == nil {
			cm.DeliveryCount = n
		}
		out = append(out, cm)
	}
	if len(out) == 0 {
		return nil, ErrEmpty
	}
	return out, nil
}

// deliveryCount reads how many times an entry has been delivered, from the
// pending-entries list.
func (q *ValkeyQueue) deliveryCount(ctx context.Context, messageID string) (int64, error) {
	pending, err := q.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: q.stream,
		Group:  q.group,
		Start:  messageID,
		End:    messageID,
		Count:  1,
	}).Result()
	if err != nil || len(pending) == 0 {
		return 1, err
	}
	return pending[0].RetryCount, nil
}

// Depth reports entries never delivered to a consumer: stream length minus
// everything the group has already handed out.
func (q *ValkeyQueue) Depth(ctx context.Context) (int64, error) {
	groups, err := q.client.XInfoGroups(ctx, q.stream).Result()
	if err != nil {
		// A missing stream is a depth of zero, not an error: it simply means
		// nothing has been enqueued since it was last trimmed away.
		if errors.Is(err, redis.Nil) || strings.Contains(err.Error(), "no such key") {
			return 0, nil
		}
		return 0, fmt.Errorf("queue depth: %w", err)
	}
	for _, g := range groups {
		if g.Name == q.group {
			return g.Lag, nil
		}
	}
	return 0, nil
}

// Pending reports entries claimed but not acknowledged.
func (q *ValkeyQueue) Pending(ctx context.Context) (int64, error) {
	res, err := q.client.XPending(ctx, q.stream, q.group).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, fmt.Errorf("queue pending: %w", err)
	}
	return res.Count, nil
}

// Ping verifies connectivity.
func (q *ValkeyQueue) Ping(ctx context.Context) error {
	return q.client.Ping(ctx).Err()
}

// Close releases the connection pool.
func (q *ValkeyQueue) Close() error {
	return q.client.Close()
}

// Len reports the total entries in the stream, which is what XLEN returns.
//
// Distinct from Depth: Depth is the consumer group's lag — work not yet handed
// to anyone — while Len includes acknowledged entries still inside the MAXLEN
// window. Depth is what tells you whether workers are keeping up; Len tells you
// how much history the trim is holding.
func (q *ValkeyQueue) Len(ctx context.Context) (int64, error) {
	n, err := q.client.XLen(ctx, q.stream).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, fmt.Errorf("queue length: %w", err)
	}
	return n, nil
}

// toClaimedMessage converts a stream entry, rejecting malformed ones.
func toClaimedMessage(msg redis.XMessage, deliveryCount int64) (ClaimedMessage, error) {
	raw, ok := msg.Values[fieldEventID].(string)
	if !ok {
		return ClaimedMessage{}, fmt.Errorf("entry %s has no %s field", msg.ID, fieldEventID)
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return ClaimedMessage{}, fmt.Errorf("entry %s has an unparseable event id %q: %w", msg.ID, raw, err)
	}

	// Strip the namespace back off so the propagator sees the keys it wrote.
	var traceFields map[string]string
	for k, v := range msg.Values {
		if !strings.HasPrefix(k, traceFieldPrefix) {
			continue
		}
		sv, ok := v.(string)
		if !ok {
			continue
		}
		if traceFields == nil {
			traceFields = make(map[string]string, 2)
		}
		traceFields[strings.TrimPrefix(k, traceFieldPrefix)] = sv
	}

	return ClaimedMessage{
		MessageID:     msg.ID,
		EventID:       id,
		DeliveryCount: deliveryCount,
		TraceFields:   traceFields,
	}, nil
}

// compile-time check
var _ Queue = (*ValkeyQueue)(nil)

// StreamKeyFor exposes a queue's stream key. Used by tests that need to
// manipulate the underlying stream directly to simulate a Valkey wipe.
func StreamKeyFor(q *ValkeyQueue) string { return q.stream }
