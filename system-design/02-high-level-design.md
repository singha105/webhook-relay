# High-level design

## System context

Who talks to what, and where the trust boundaries are.

```mermaid
flowchart LR
    subgraph ext1[" "]
        PROD["Producer application<br/><small>the customer of this system</small>"]
    end
    subgraph sys["webhook-relay — the system under design"]
        RELAY["Ingest · store · schedule · deliver · record"]
    end
    subgraph ext2[" "]
        RECV["Third-party HTTP endpoints<br/><small>outside our control</small>"]
        OPS["Operators<br/><small>dashboards, alerts, runbook</small>"]
    end

    PROD -->|"POST /v1/events"| RELAY
    PROD -->|"GET status, replay"| RELAY
    RELAY -->|"signed POST, retried"| RECV
    RELAY -->|"metrics · logs · traces"| OPS

    style RELAY fill:#1a1d21,color:#fff
    style RECV fill:#2d7d46,color:#fff
```

The asymmetry is the whole design problem: we control everything inside the box
and nothing to the right of it. Receivers cannot be fixed, restarted, or
reasoned with — only accommodated.

## Component view

```mermaid
flowchart LR
    P[Producer]

    subgraph relay["webhook-relay"]
        API["API<br/><small>chi · stdlib http</small>"]
        PG[("PostgreSQL 16<br/><small>source of truth</small>")]
        R["Outbox relay<br/><small>sole queue producer</small>"]
        VK[("Valkey 8<br/><small>Streams · consumer group</small>")]
        W["Delivery workers<br/><small>N goroutines</small>"]
    end

    subgraph guards["per-delivery guards"]
        RL["Token bucket<br/><small>Lua, atomic</small>"]
        CB["Circuit breaker"]
        DD["Dedup guard<br/><small>SETNX event:attempt</small>"]
    end

    EP["Customer endpoint"]

    P -->|"POST /v1/events<br/>202 once durable"| API
    API -->|"one transaction"| PG
    R -->|"claim batch<br/>FOR UPDATE SKIP LOCKED"| PG
    R -->|"XADD pipelined"| VK
    VK -->|"XREADGROUP"| W
    W --> RL --> CB --> DD
    DD -->|"POST + HMAC-SHA256"| EP
    W -->|"record attempt"| PG
    W -->|"XACK"| VK
    VK -.->|"XAUTOCLAIM<br/>recover dead workers"| W

    style PG fill:#336791,color:#fff
    style VK fill:#c6302b,color:#fff
    style EP fill:#2d7d46,color:#fff
```

The shape that matters: **ingest never touches the queue.** It writes to
Postgres in one transaction and returns 202. A separate relay polls for due
events and is the only producer the queue has. That is the transactional outbox
pattern, and it is what makes the 202 mean something precise — "durably stored"
— rather than "probably stored and probably queued"
([ADR 0002](../docs/adr/0002-transactional-outbox.md)).

The dashed arrow is the one that makes the system survivable. When a worker
dies holding entries, `XAUTOCLAIM` hands them to a live worker after the stale
timeout. It is also the arrow that causes duplicate deliveries, which is what
the [postmortem](../docs/postmortem-duplicate-delivery.md) is about.

## Responsibilities

Each component has exactly one job, and the boundaries are enforced by what
each is *allowed to touch*.

| Component | Owns | Deliberately cannot |
|---|---|---|
| **API** (`cmd/api`) | Validation, idempotency, one transactional write, read APIs | Touch the queue, or perform any delivery work |
| **Outbox relay** (`internal/relay`) | Claiming due events, leasing, enqueueing, sweeping expired leases | Send HTTP to receivers |
| **Queue** (Valkey Streams) | Handing ready work to exactly one worker; tracking in-flight ownership | Be a system of record — it is a transport |
| **Worker pool** (`internal/worker`) | Guards, signing, the HTTP call, recording outcomes, acking | Invent work; it only processes what the relay produced |
| **Guards** (ratelimit, breaker, dedup) | Deciding whether *this* delivery may proceed right now | Persist anything that must survive Valkey loss |
| **Postgres** | Every fact that must survive a crash | — |

## Request lifecycle

```mermaid
sequenceDiagram
    autonumber
    participant P as Producer
    participant A as API
    participant DB as Postgres
    participant R as Relay
    participant Q as Valkey
    participant W as Worker
    participant E as Endpoint

    P->>A: POST /v1/events
    A->>DB: INSERT … ON CONFLICT (idempotency)
    DB-->>A: event row
    A-->>P: 202 Accepted
    Note over A,P: 202 means durably stored, nothing more

    loop every 250ms
        R->>DB: claim batch, stamp lease
        R->>Q: XADD (pipelined)
    end

    W->>Q: XREADGROUP
    W->>DB: load endpoint + event
    W->>W: breaker → rate limit → dedup
    W->>E: POST + HMAC-SHA256
    E-->>W: 2xx / 5xx / timeout
    W->>DB: record attempt, update status
    W->>Q: XACK
    Note over W,Q: crash between the POST and XACK = duplicate
```

## Deployment topology

Two independent paths, both committed, both runnable:

| | Local (`make demo`) | Kubernetes (`make demo-k8s`) |
|---|---|---|
| Orchestration | docker-compose | k3d cluster, Helm chart |
| Postgres | single container | CloudNativePG operator |
| Delivery | — | ArgoCD, pull-based ([ADR 0005](../docs/adr/0005-gitops-pull-over-push.md)) |
| Secrets | `.env`, gitignored | Sealed Secrets ([ADR 0006](../docs/adr/0006-sealed-secrets-over-cloud-kms.md)) |
| Scaling | fixed | HPA on CPU; queue-depth metric available but off |
| Status | **fully verified** | verified on the single-node profile ([#21](https://github.com/singha105/webhook-relay/issues/21)) |

## Design constraints and their consequences

| Choice | Bought | Cost |
|---|---|---|
| Outbox over dual-write | 202 means exactly one thing; queue loss is survivable | ≤250ms added latency; relay is a serial chokepoint |
| At-least-once | Never loses an accepted event | Receivers must dedupe on `X-Webhook-Id` |
| Valkey Streams over Kafka | No JVM, no ZooKeeper, one extra process | Single-threaded; depth metric untruthful above `MAXLEN` |
| Postgres as system of record | One place to look; trivially consistent | All delivery state transitions hit one primary |
| Per-endpoint rate limit | Protects receivers | Does **not** protect against one tenant monopolising workers |

## Scale characteristics

Measured on one laptop (see [Performance](../README.md#performance)): **~875 events/sec in,
~870/sec out**. The known limits, in the order they would bite:

1. **One shared queue** — a slow receiver occupies workers everyone else needs.
   The first thing to fix; see [What I would do differently](#what-i-would-do-differently-at-scale).
2. **The relay is serial** — sole producer for the whole system. Batching its
   enqueues bought 5.4%; it remains a single point of throughput.
3. **A single Postgres primary** absorbs every state transition.
4. **An unidentified serialization point** — 5× the workers buys 7% on an
   unsaturated machine ([#6](https://github.com/singha105/webhook-relay/issues/6)).

---


## Scale and trade-offs

### What I would do differently at scale

Everything below is deliberately *not* built. This system targets a single node
and hundreds of events per second; each of these is the right answer at a scale
it does not have, and building them now would be architecture cosplay.

**Partition the queue by endpoint.** One stream and one consumer group means a
single slow receiver occupies workers that everyone else is waiting for — the
noisy-neighbour problem, and the most urgent item here. A receiver that takes
9s per request ties up a worker for 9s, and with concurrency 10 it takes ten
such receivers to stall the system entirely. Partitioning by `endpoint_id` hash
into per-partition consumer groups bounds the damage to one partition. The cost
is rebalancing when endpoints are added, and hot partitions when one customer
dwarfs the rest.

**Separate worker pools per priority tier.** Password resets and marketing
webhooks currently share a queue. They should not: one is user-visible and
latency-critical, the other can wait minutes. Separate pools with separate
concurrency budgets stop a bulk backfill from delaying a login email. The cost
is capacity planning per tier and deciding what happens when the high-priority
pool is idle while the low one is saturated.

**Move the outbox relay to CDC via Debezium.** The relay polls every 250ms,
which adds latency to every event and puts a floor under how fresh delivery can
be. Debezium reading the Postgres WAL turns that into a push: the event is in
the queue milliseconds after commit, with no polling and no lease bookkeeping.
It also removes the relay as a serialization point entirely. The cost is
Kafka Connect, a Kafka cluster, and replication-slot management — a large
operational commitment, which is exactly why it is not here at this size.

**Shard Postgres by endpoint.** One primary handles ingest and every delivery
state transition. At ~10× current write volume the delivery-attempt table
becomes the constraint long before ingest does. Sharding by `endpoint_id` keeps
each endpoint's events and attempts colocated, so no query needs a scatter-gather.
The cost is that cross-shard operations — global dead-letter listings, aggregate
metrics — become fan-out queries, and resharding is a project rather than a
config change.

**Per-tenant fair queueing.** Rate limiting is per-endpoint, which protects
*receivers* and does nothing to stop one tenant consuming all delivery capacity.
A tenant posting a million events monopolises the workers no matter how polite
each individual delivery is. Weighted fair queueing or deficit round-robin
across tenant queues fixes it. The cost is that fairness needs a scheduler, and
a scheduler needs to know tenant weights, which is a product decision before it
is an engineering one.

---

---

**Previous:** [Problem statement](01-problem-statement.md) · **Next:** [Low-level design →](03-low-level-design.md)
