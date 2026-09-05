# 1. Valkey Streams as the queue

**Status:** Accepted

## Context

Delivery work has to be handed from the ingest path to a pool of workers that
can crash at any time. The queue needs four things:

1. **Consumer groups** — many workers, each entry going to exactly one of them.
2. **Recovery of in-flight work.** A worker that dies holding an entry must not
   take that entry with it. This is the requirement that eliminates most
   options.
3. **No new operational surface.** A rate limiter and a circuit breaker already
   need a fast shared data store, so something Redis-shaped is in the stack
   regardless.
4. **Free and self-hosted.** No managed service, no account.

The realistic alternatives were Kafka, RabbitMQ, NATS JetStream, and
Postgres-as-a-queue (`SELECT … FOR UPDATE SKIP LOCKED`).

## Decision

Valkey Streams with consumer groups: `XADD` to produce, `XREADGROUP` to claim,
`XACK` to complete, `XAUTOCLAIM` to recover entries whose owner died.

Valkey rather than Redis because Redis changed to a source-available licence in
2024; Valkey is the Linux Foundation fork and is a drop-in replacement.

## Why not the alternatives

**Kafka** is the correct answer at a scale this project does not have. It buys
partitioned ordering and long retention, and costs a ZooKeeper-or-KRaft
cluster, a schema story, and several gigabytes of JVM. Bringing it in for
~900 events/sec on a laptop would be cargo cult.

**RabbitMQ** fits the workload well and I would not argue against it. It loses
on requirement 3: it is a second stateful system to run, monitor and reason
about, when the stack already contains one that can do the job.

**NATS JetStream** is the closest call — lighter than Kafka, better ergonomics
than Rabbit. It lost on the same requirement, plus familiarity: I can debug
Valkey with `redis-cli` and I know what its failure modes look like.

**Postgres as the queue** would have removed a component entirely and made the
outbox unnecessary — the enqueue and the insert become one transaction, which
is genuinely simpler. It was rejected because polling `FOR UPDATE SKIP LOCKED`
puts the delivery hot path on the same disk and the same connection pool as
ingest, so a delivery backlog degrades the ingest latency the SLO is written
against. Separating them is the point.

## Consequences

**Good.** Recovery is a first-class operation rather than something built by
hand: `XAUTOCLAIM` on entries idle longer than the stale timeout is the whole
mechanism. The pending-entries list is directly inspectable, so "how many
deliveries are in flight and who owns them" is one command. `DeliveryCount`
per entry distinguishes "this endpoint keeps failing" from "our workers keep
dying holding this job" — different incidents that look identical in aggregate
metrics.

**Bad.** Valkey is single-threaded, so it is a shared serial resource. Measured
at ~33% of one core at ~870 events/sec, which is fine here and is a ceiling
worth remembering.

**Bad.** Stream trimming is a real constraint, not a detail. `MAXLEN ~ 100000`
means `webhook_queue_depth` stops being a truthful backlog metric above that
bound — during the Day 5 ramp it pinned at exactly 100,018 and simply stopped
counting. A dashboard reading that as "the backlog is 100k" would be wrong.

**Bad.** Persistence is Valkey's, not Postgres's. If Valkey loses data, queue
entries vanish — which is survivable only because of
[ADR 0002](0002-transactional-outbox.md): Postgres remains the source of truth
and the relay re-enqueues anything whose lease expires. The queue is a
transport, never a system of record.

**Sharp edge.** A consumer group destroyed underneath running workers produces
`NOGROUP`, and workers wedge on it forever unless it is explicitly handled. It
was found the hard way, by flushing Valkey during Day 3, and the recovery path
now exists because of that.
