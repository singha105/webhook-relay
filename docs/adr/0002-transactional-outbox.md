# 2. Transactional outbox for enqueueing

**Status:** Accepted

## Context

Ingest has to do two things: persist the event and make it available for
delivery. They live in two different systems — Postgres and Valkey — and there
is no transaction spanning both.

Doing them naively means picking which failure you prefer:

- **Insert, then enqueue.** Crash in between and the event exists but is never
  delivered. A silently dropped webhook, invisible until a customer asks where
  it went.
- **Enqueue, then insert.** Crash in between and a worker claims a message for
  a row that does not exist. Recoverable — the worker discards it — but now the
  queue can reference events that were never accepted, and the API has returned
  202 for something it did not store.

Neither is acceptable for a system whose entire value proposition is "we will
deliver your webhook."

## Decision

The ingest path writes **only to Postgres**, inside one transaction. It never
touches the queue.

A separate relay polls for events that are due, claims a batch with
`UPDATE … SET status='delivering' … RETURNING` under `FOR UPDATE SKIP LOCKED`,
and enqueues them. The relay is the **sole producer** for the entire system.

## Consequences

**Good.** The 202 means exactly one thing: the event is durably in Postgres.
That is a promise the API can keep with a single transaction, and no
distributed-commit reasoning is required to justify it.

**Good.** Postgres is unambiguously the source of truth. Valkey can be flushed,
lost, or replaced and no event is lost — the relay re-enqueues anything whose
lease expires. This was verified rather than assumed (chaos experiment 5).

**Good.** `FOR UPDATE SKIP LOCKED` means several relay replicas can run without
coordination; they take disjoint batches instead of contending or
double-enqueueing.

**Bad — the cost that actually hurts.** Latency. An event waits up to one poll
interval (250ms) before a worker ever sees it. For a webhook relay that is
irrelevant; for anything interactive it would be disqualifying. `EnqueueNow`
exists as a bypass for the demo path, and it is a bypass, not a general answer:
it can still lose the enqueue, and the relay is what makes that survivable.

**Bad.** The relay is a single serial component every event must pass through,
which makes it a natural bottleneck. It was: until Day 5 it enqueued one event
per round trip in a serial loop, O(n) per batch. Pipelining bought 5.4%, and
the fact that it was *only* 5.4% is itself recorded in
[loadtest/README.md](../../loadtest/README.md).

**Bad.** Two components can now claim an event — the relay's lease and the
worker's queue entry — so "what is this event's real state" requires reading
both. `next_retry_at` doubling as the lease expiry while `delivering` is the
compromise that keeps this to one column instead of three.

**Deliberately unfixed.** The relay adds an at-least-once step of its own: a
crash after `XADD` but before the claim is committed re-enqueues the event.
That is fine, because delivery is already at-least-once
([ADR 0004](0004-at-least-once-over-exactly-once.md)) and the receiver must
tolerate duplicates regardless. Adding exactly-once machinery *here* would buy
nothing while the HTTP boundary remains at-least-once.
