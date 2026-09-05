# webhook-relay

[![CI](https://github.com/singha105/webhook-relay/actions/workflows/ci.yml/badge.svg)](https://github.com/singha105/webhook-relay/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A webhook delivery service — the thing that sits between your application and
your customers' HTTP endpoints and makes sure events arrive, retrying when they
do not. Think the delivery half of Stripe webhooks or Svix.

It ingests events at **~875/sec**, delivers at **~870/sec**, signs every
payload with HMAC-SHA256, retries with full-jitter exponential backoff, dead
letters after six attempts, and survives having its workers killed mid-flight.
Those numbers come from a load test you can re-run with one command on the
hardware described in [Performance](#performance) — a laptop, not a cluster.
Everything runs locally: Postgres, Valkey, Kubernetes, and the whole
observability stack are self-hosted with no cloud account anywhere.

![make demo](docs/images/demo.gif)

*The real `make demo`, time-compressed 3×. Stack up, signed webhook delivered,
a failing receiver driven through the retry ladder into the DLQ and replayed,
then a worker SIGKILLed with ten deliveries in flight.*

---

> ### 📄 Start here: [Postmortem — duplicate webhook deliveries](docs/postmortem-duplicate-delivery.md)
>
> I killed a worker mid-delivery and measured what broke. Every request in
> flight was delivered **twice** — and the number of duplicates turned out to
> be exactly the worker's concurrency setting, which means the throughput knob
> is also the correctness blast radius. The write-up covers why acking after a
> side effect makes this unavoidable, and why no metric we export would have
> caught it in production.
>
> Second one: [deliveries the receiver completed and we recorded as
> failures](docs/postmortem-timeout-boundary.md) — a receiver 400ms slower than
> our timeout processed 15 requests while we recorded zero.

---

## Quickstart

Four commands from nothing to a running system with events flowing through it:

```bash
git clone https://github.com/singha105/webhook-relay.git
cd webhook-relay
make demo
```

`make demo` builds the images, starts the stack, applies migrations, delivers a
signed webhook, drives a receiver to failure through the full retry ladder into
the dead letter queue, replays it, then **SIGKILLs a worker with deliveries in
flight** and shows what survives. Takes about five minutes on a cold cache.

Requires Docker, `curl`, and `jq`. Nothing else — no account, no API key, no
cloud provider.

```bash
make down          # stop everything, delete the volumes
make chaos-list    # the ten chaos experiments
make help          # everything else
```

---

## Architecture

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
([ADR 0002](docs/adr/0002-transactional-outbox.md)).

The dashed arrow is the one that makes the system survivable. When a worker
dies holding entries, `XAUTOCLAIM` hands them to a live worker after the stale
timeout. It is also the arrow that causes duplicate deliveries, which is what
the [postmortem](docs/postmortem-duplicate-delivery.md) is about.

---

## Delivery semantics

**Every event is delivered at least once. Some events will be delivered more
than once. Receivers must deduplicate on `X-Webhook-Id`.**

That is the whole contract, and the rest of this section explains why it cannot
be better.

### Why not exactly-once

Exactly-once delivery over a lossy network is not hard — it is **impossible**,
and the impossibility has a name: the **two generals' problem**.

Two generals must agree to attack, communicating only by messengers who may be
captured. A sends "attack at dawn". Did it arrive? A needs an acknowledgement.
B sends one. Did *that* arrive? B now needs an acknowledgement of the
acknowledgement. The regress never terminates. No finite protocol over a lossy
channel gives both parties common knowledge that a message was received.

Our channel is HTTP over the internet. When a delivery times out, two worlds
are consistent with everything we can observe:

1. The request never arrived; the receiver did nothing.
2. The request arrived, the receiver processed it, and the **response** was lost.

Nothing on our side distinguishes them, so we must choose:

| Choice | Guarantee | Failure mode |
|---|---|---|
| Assume it failed, retry | **at-least-once** | receiver may process twice |
| Assume it succeeded, stop | at-most-once | events silently lost |

We choose at-least-once, because a duplicate is loud and fixable by an
idempotent receiver, while a lost webhook is silent and unrecoverable.

This is not theoretical. [Chaos experiment
10](docs/postmortem-timeout-boundary.md) put a receiver 400ms on the wrong side
of our timeout: **it successfully processed 15 requests while we recorded zero
deliveries.** Both sides behaved correctly. The channel ate the answer.

Anything advertising "exactly-once" is doing at-least-once plus deduplication
at the receiver. That is what we ask for directly, instead of selling it as a
guarantee we cannot make ([ADR 0004](docs/adr/0004-at-least-once-over-exactly-once.md)).

### What we actually guarantee

1. **Durability before acknowledgement.** A 202 means the event is committed to
   Postgres. Not queued, not in memory — committed.
2. **At-least-once delivery**, or a dead letter after six attempts, visible via
   the API. Never silently dropped.
3. **A stable `X-Webhook-Id`** across every retry of an event. This is your
   deduplication key.
4. **Signed payloads.** HMAC-SHA256 over `{timestamp}.{raw body}`, compared with
   `hmac.Equal`. A retry is verifiable as ours.
5. **Ordering is not guaranteed.** Events are delivered concurrently and retries
   reorder them. If you need ordering, use the event's own sequence data.

### What we do not guarantee, and why it is narrower than it sounds

The delivery-side dedup guard suppresses a duplicate *dispatch* of the same
`(event_id, attempt)`. Measured: **10 duplicates without it, 0 with it, across
400 events under a hard kill** — over a three-minute window.

Over a longer window it is weaker than that sounds. The guard **defers** the
duplicate rather than removing it: a worker that suppresses a dispatch acks the
queue entry without recording an outcome, so the event sits in `delivering`
until the marker expires and is then delivered again. Measured with a
compressed clock, all 8 in-flight requests duplicated 73 seconds after the kill
([#19](https://github.com/singha105/webhook-relay/issues/19)).

It does not — and cannot — prevent a duplicate caused by a **timeout**, because
a timeout produces a legitimately new attempt number that *should* be retried.
That is exactly the case where the receiver already did the work. The window is
narrowed, not closed, and it cannot be closed from the sender's side.

---

## Performance

Measured on the machine this was built on. Single-node, laptop hardware, and
the numbers say so — a defensible small number beats an inflated one.

| | |
|---|---|
| Machine | Apple M3, 8 cores, 8 GB RAM |
| Docker | 3.825 GiB, 8 CPUs |
| Stack | docker-compose (Postgres 16, Valkey 8, api, worker, sink) |
| Load generator | k6, on the same machine |

| Metric | Result |
|---|---|
| Ingest, sustained | **~875 events/sec** |
| Delivery, sustained | **~870 events/sec** |
| Ingest p95 | **< 50 ms** (threshold; the run fails if exceeded) |
| Error rate | **< 0.1%** (threshold) |

```bash
make demo-up                        # stack + controllable receiver
k6 run loadtest/baseline.js         # 200/s for 5 minutes
./loadtest/drain-throughput.sh      # sustained delivery throughput
```

### The bottleneck hunt

The first three hypotheses were wrong, which is the useful part:

| Hypothesis | Verdict |
|---|---|
| pgx pool too small (10 → 25) | no effect |
| worker concurrency too low (10 → 50) | no effect |
| both together (50 / pool 60) | no effect — 750 vs 769 |
| some saturated resource | **nothing was saturated** |

`docker stats` during a drain, against 800% available: worker 123.7%, Postgres
123.6%, Valkey 32.6%. Nothing CPU-bound, so the system is **latency-bound on
sequential round trips** rather than short of capacity — which ruled out "add
more workers" and pointed at round-trip count instead.

The relay is the sole producer for the entire system, and it was enqueueing one
event per round trip in a serial loop — O(n) `XADD`s per batch.

| Configuration | Median |
|---|---|
| Baseline (serial enqueue) | 769.2 events/sec |
| Pipelined enqueue | **810.8 events/sec** (+5.4%) |
| Pipelined + concurrency 50 / pool 60 | **869.6 events/sec** (+13.1%) |

**The honest caveat:** 5× the concurrency still buys only 7% on an unsaturated
machine, so a serialization point remains that I did not isolate. It is
[issue #6](https://github.com/singha105/webhook-relay/issues/6) with ranked
candidates, not a mystery I am pretending is solved.

Three experiments during that day turned out to be measuring nothing at all —
an env var not plumbed through compose, a harness timing only the tail, and
`$COMPOSE up -d` silently doing nothing in zsh. All three are written up rather
than deleted.

**Raw data:** [`loadtest/README.md`](loadtest/README.md) ·
[`loadtest/results/`](loadtest/results/) ·
[optimization before/after](loadtest/results/optimization-relay-pipelining.md)

---

## Chaos experiments

Eleven experiments, each with a prediction and a falsification criterion committed
**before** it ran, so they cannot be retrofitted to results.

| # | Experiment | Hypothesis | What actually happened |
|---|---|---|---|
| 1 | Kill a worker mid-delivery | Entries are reclaimed; no loss | Covered by #2 |
| 2 | Same, dedup disabled | Duplicates appear without the guard | ✅ **Correct.** 10 duplicates without, 0 with, over 400 events. Duplicates = in-flight = concurrency = 10 |
| 3 | Poison message | No payload can wedge a worker | ✅ **Correct.** 10 of 11 hostile payloads delivered, malformed JSON rejected at ingest, 0 restarts |
| 4 | 30s latency vs 10s timeout | Timeouts, retries, no loss | ⬜ Not run — needs Chaos Mesh |
| 5 | Kill Valkey | Queue lost, Postgres re-enqueues, no events lost | ⬜ Script committed, not yet run |
| 6 | Kill the Postgres primary | CNPG fails over; writes resume | ⬜ Not run — needs 3 replicas |
| 7 | Workers to zero for 5 min | Backlog grows, drains on return | ⬜ Not run — needs Chaos Mesh |
| 8 | Partition worker from Valkey | Worker stalls, recovers | ⬜ Not run — needs Chaos Mesh |
| 9 | CPU stress on a worker | Throughput drops, no incorrectness | ⬜ Not run — needs Chaos Mesh |
| 10 | Receiver at the timeout boundary | Double-processing at the receiver | ❌ **Prediction wrong.** At 9.9s vs a 10s timeout: zero timeouts, perfectly clean. I approximated the boundary instead of crossing it. At 10.4s: receiver processed 15 requests, we recorded **0 deliveries** |
| 11 | What happens to a suppressed event | The guard defers the duplicate rather than preventing it | ✅ **Correct, and it corrects #2.** All 8 in-flight requests duplicated 73s after the kill, once the dedup marker expired |

Five did not run. Chaos Mesh needs ~6 GiB of Docker memory and this machine has
3.825 GiB, so their manifests and predictions are committed **unresolved**
rather than given invented results. An experiment log where every prediction was
confirmed is a log where nothing was learned.

Full predictions and results: [`docs/chaos-results.md`](docs/chaos-results.md) ·
manifests in [`chaos/`](chaos/)

---

## What I would do differently at scale

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

## Known limitations

Named here rather than left for a reviewer to find.

- **Events stranded in `delivering` after a worker dies.** When the dedup guard
  suppresses a re-dispatch, the worker acks the queue entry without recording an
  outcome, so the event sits in `delivering` until the lease sweep and the dedup
  TTL let it through. Found by `make demo` on Day 6, not by a test.
  [#19](https://github.com/singha105/webhook-relay/issues/19)
- **`/readyz` reports ready with no schema.** It checks that Postgres is
  reachable, not that migrations have run, so a freshly deployed pod passes its
  health check and then 500s every request.
  [#18](https://github.com/singha105/webhook-relay/issues/18)
- **A serialization point in delivery is unidentified.** 5× the worker
  concurrency buys 7% on a machine where nothing is CPU-saturated.
  [#6](https://github.com/singha105/webhook-relay/issues/6)
- **Half the chaos experiments have not run** — five of ten need Chaos Mesh or a
  multi-replica Postgres, neither of which fits in 3.825 GiB.
- **No authentication.** Anyone who can reach the API can register endpoints and
  post events. Real deployments need per-tenant API keys.
- **No SSRF protection.** Endpoint URLs may point at loopback and private
  ranges — required for local testing, unacceptable hosted without an egress
  allowlist.
- **No list/filter endpoint for events.** Only get-by-id. Operational queries go
  through psql, which is why `make demo` shells into Postgres for its counts.
- **`endpoint_id` is a metric label** — one series per endpoint on four metrics.
  Fine at tens of endpoints, a cardinality problem at thousands.
- **Delivery is at-least-once, deliberately.** See [Delivery
  semantics](#delivery-semantics).
- **`payload: null` is accepted** and delivered as the four bytes `null`, since
  it is valid JSON. [#7](https://github.com/singha105/webhook-relay/issues/7)
- **NetworkPolicies are written but not enforced**, because k3s ships Flannel.
  They need Calico or Cilium to do anything.
- **The HPA's queue-depth metric is disabled by default.** It needs
  prometheus-adapter, which is not installed. An HPA referencing a metric nobody
  serves is a silently degraded autoscaler, so it is off rather than aspirational.
- **Grafana runs with anonymous admin access** in the Compose stack. Correct for
  a local demo, not something to deploy; the Kubernetes path uses a generated
  password.
- **Tracing samples at 100%.** Right at this volume, wrong for real traffic.
- **The breaker can overshoot its threshold** — concurrent goroutines can push
  `consecutive_failures` past the limit before the open takes effect. Deliberate:
  an exact trip would need a lock on the hot path. The *probe* is exact.
- **Offset pagination** on `GET /v1/endpoints` drifts under concurrent inserts.
  Endpoints are registered by humans, so this is acceptable; the events table
  would need keyset pagination.
- **Queue depth stops being truthful above 100,000.** The stream is trimmed at
  `MAXLEN ~ 100000`, so the gauge pins there and stops counting.

---

## Documentation

| | |
|---|---|
| [Postmortem: duplicate deliveries](docs/postmortem-duplicate-delivery.md) | The headline incident, with measured evidence |
| [Postmortem: the timeout boundary](docs/postmortem-timeout-boundary.md) | Deliveries that succeeded and we recorded as failures |
| [Architecture decision records](docs/adr/) | Six decisions and what each one costs |
| [Chaos results](docs/chaos-results.md) | Predictions vs outcomes, including the wrong one |
| [Load testing](loadtest/README.md) | The bottleneck hunt and three invalid experiments |
| [Runbook](docs/runbook.md) | On-call procedures |
| [Secrets](docs/secrets.md) | Sealed Secrets, and the key-loss failure mode |
| [Contributing](CONTRIBUTING.md) | Conventions and local setup |

---

## API

| Method | Path | Returns |
|---|---|---|
| `POST` | `/v1/endpoints` | `201` + the signing secret, once |
| `GET` | `/v1/endpoints` | `200` list (no secrets) |
| `GET` | `/v1/endpoints/{id}` | `200` or `404` |
| `PATCH` | `/v1/endpoints/{id}` | `200`, only the fields you send |
| `DELETE` | `/v1/endpoints/{id}` | `204`, cascades to events and attempts |
| `POST` | `/v1/events` | `202` accepted, or `200` on an idempotent replay |
| `GET` | `/v1/events/{id}` | `200` status + full attempt history |
| `POST` | `/v1/events/{id}/replay` | `202` requeued with a fresh budget, or `409` if not terminal |
| `GET` | `/healthz` | liveness — touches no dependency |
| `GET` | `/readyz` | readiness — pings Postgres |

Every error uses one envelope, so a client writes one error handler:

```json
{
  "error": {
    "code": "validation_failed",
    "message": "the request body failed validation",
    "fields": [
      { "field": "url", "message": "must use http or https scheme" },
      { "field": "rate_limit_per_sec", "message": "must be between 1 and 1000" }
    ],
    "request_id": "b291971c-96e2-43f5-acf0-c44847eab9ce"
  }
}
```

Validation reports **every** broken field at once, so fixing a bad request does
not take one round trip per mistake. The `request_id` matches the one on every
log line for that request, and is echoed in the `X-Request-ID` response header.

---

---

## Design decisions

The parts worth arguing about, and what each one costs.

### The idempotency race

Ingest never does `SELECT`-then-`INSERT`. That pattern has a window between the
two statements where a concurrent request can insert the same key, and closing
it in application code needs a distributed lock nobody wants on the hot path.

Instead the `INSERT` runs unconditionally against a partial unique index and
Postgres arbitrates:

```sql
INSERT INTO events (id, endpoint_id, event_type, payload, idempotency_key)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (endpoint_id, idempotency_key) WHERE idempotency_key IS NOT NULL
DO UPDATE SET idempotency_key = events.idempotency_key
RETURNING ...
```

`DO UPDATE`, not `DO NOTHING`, even though the update writes the key back to
itself. `DO NOTHING` returns zero rows on conflict, which forces a follow-up
`SELECT` that may run *before the winning transaction has committed* — the race
just moves one statement later. `DO UPDATE` takes the row lock, waits for the
winner, and hands back the surviving row.

**What it costs:** one dead tuple per duplicate, for vacuum to reclaim.

"Was it created?" is answered by comparing the returned ID to the UUIDv7 we
generated — the database can only return our ID if our insert was the one that
landed. That avoids the `xmax = 0` trick and its dependence on a system column.

### The transactional outbox

Ingest has to do two things — persist the event, and make it deliverable — and
they live in two systems with no transaction spanning them. Whichever order you
pick, a crash between them breaks something:

| Order | Crash in between |
|---|---|
| Enqueue, then insert | A message pointing at an event that does not exist. |
| Insert, then enqueue | An event nothing will ever deliver. **Silent loss** — nothing reports an error. |

The outbox removes the choice. **Ingest writes only to Postgres**, in one
transaction, and returns `202`. A separate relay is the queue's only producer,
and it derives what to enqueue from the durable rows. A crash can then only
cause a *duplicate* enqueue, never a lost event — and duplicates are already
inside the at-least-once contract.

**What it costs:** an event waits up to one relay poll (250ms by default)
before it is enqueued. That is the price of not lying about atomicity.

The relay claims with a **lease** rather than holding a transaction open across
the enqueue. The textbook version — `BEGIN`, `SELECT … FOR UPDATE SKIP LOCKED`,
`XADD`, `UPDATE`, `COMMIT` — holds Postgres row locks across a network round
trip to a different system. Under load that turns every Valkey hiccup into lock
contention and a Valkey timeout into a long transaction blocking vacuum.
Instead the claim and the lease happen in one statement; what happens if the
relay dies mid-batch is enumerated case by case in
[`ClaimDueEvents`](internal/store/delivery.go).

### Valkey Streams, not a LIST and not SKIP LOCKED

A stream with a consumer group is the only one of the three that can express
*"this worker is holding this job right now, and if it dies somebody else must
get it."*

- **Not a LIST.** `LPOP` hands the job over and forgets it. Kill that worker
  between the pop and the HTTP call — which is exactly what Day 5 does — and the
  job is gone with no record it existed. `BRPOPLPUSH` into per-worker processing
  lists gets partway, but recovery then means enumerating every worker's list
  and deciding which owners are still alive: a consumer group rebuilt by hand.
- **Not Postgres `SKIP LOCKED`**, though the relay itself uses it. Every polling
  worker would hold a connection for its whole transaction, bounding worker
  count by `max_connections`, and queue churn would land on the same disk as the
  durable event log.
- **Streams** keep a per-group pending-entries list. `XREADGROUP` moves an entry
  there under a named consumer until `XACK`; `XAUTOCLAIM` reassigns entries idle
  too long. That is the recovery primitive, not something we build.

**What we give up:** the pending list is memory-resident; there is no delayed
redelivery, so backoff lives in Postgres; and it is at-least-once, never
exactly-once.

### Two independent recovery paths

A worker can die holding a job. So can Valkey.

| Failure | Recovered by |
|---|---|
| Worker dies holding a claim | `XAUTOCLAIM` after `STALE_CLAIM_TIMEOUT` (60s) |
| Worker dies **and** Valkey loses the pending list | The Postgres lease expiring after `DELIVERY_LEASE` (5m) |

The second is the one people forget. `XAUTOCLAIM` cannot recover an entry from a
pending list that no longer exists — a restarted pod, a flushed database, a
recreated consumer group. Only a durable lease covers both together, which is
why `next_retry_at` doubles as a lease expiry while an event is `delivering`.

These timeouts have an ordering that must hold:

```
DELIVERY_TIMEOUT  <  STALE_CLAIM_TIMEOUT  <  DELIVERY_LEASE
```

Violate the first and a slow-but-healthy delivery gets reclaimed and duplicated
while still in flight. Violate the second and the relay requeues events the
workers still hold. **Both are validated at boot**, with an error naming the
consequence — this is not the kind of thing you want to diagnose from duplicate
deliveries under load.

### Full jitter, and why it beats fixed backoff

An endpoint goes down. Every in-flight event fails at the same instant. With a
fixed delay — or with plain exponential backoff — every one of them retries at
the same instant too. The herd stays synchronized: the endpoint recovers, takes
the entire backlog as one spike, falls over, and the next round is just as
synchronized as the last.

**Backoff without jitter does not spread load. It postpones a stampede and then
reproduces it**, with a longer gap between identical spikes.

Drawing uniformly from `[0, ceiling)` breaks the correlation on the first retry
round and keeps it broken, because every subsequent draw is independent.

**What it costs:** the mean delay is halved (`E[U(0,c)] = c/2`), so clients
retry sooner on average than the ceiling implies, and any individual sequence is
not monotonically increasing. "Equal jitter" (`c/2 + U(0, c/2)`) trades some
decorrelation for a tighter lower bound; full jitter wins here because the
synchronized-herd case is exactly what Day 5 will create on purpose.

The property is [tested directly](internal/delivery/backoff_test.go), not
asserted in a comment: a thousand delays are bucketed across the window, every
bucket must be occupied, and no bucket may hold an outsized share. Without
jitter, one bucket would hold all thousand.

### Delivery-side deduplication, and its honest limits

Before dispatching, a worker claims `delivery:{event_id}:{attempt}` with
`SETNX` and a TTL. If the key exists, another worker already sent this exact
attempt.

Keyed on the **pair**, not the event: retries are supposed to happen, and only a
redelivery of the *same attempt* is a duplicate.

**This narrows the window. It cannot close it.** The gap between claiming the
key and the request reaching the receiver is still unprotected, and reversing
the order just trades one failure for the other. There is no exactly-once across
a network boundary — which is why every delivery carries a stable
`X-Webhook-Id` and [`pkg/webhook`](pkg/webhook) tells receivers to deduplicate
on it.

It also **fails open**: if Valkey is unreachable, the delivery proceeds.
Refusing to deliver because the cache is down would convert a duplicate-delivery
risk into a no-delivery outage.

`DELIVERY_DEDUP_ENABLED=false` turns it off on purpose, so Day 5 can demonstrate
the duplicates it prevents. A safety control that is never observed failing is
indistinguishable from one that does nothing.

### Retry classification

| Response | Outcome | Why |
|---|---|---|
| `2xx` | Success | — |
| `408`, `429` | Retry | Both explicitly describe a condition that will pass. |
| Other `4xx` | **Permanent — straight to DLQ** | A 404 or a rejected signature will never succeed. Burning six attempts is wasted work for us, unwanted traffic for them, and it delays an operator noticing. |
| `5xx` | Retry | The server is saying it failed, not that we did. |
| `3xx` | Retry | We do not follow redirects, so this is a misconfigured receiver — one more chance beats dead-lettering on the first surprise. |
| Transport error | Retry | A refused connection is indistinguishable from a restarting endpoint. |

`Retry-After` is honoured on 429 and 503 in both RFC 9110 forms, clamped, and
combined with our own backoff by taking the **maximum** — so a receiver cannot
shorten our backoff by sending `Retry-After: 0`.

### Redirects are not followed

Beyond a `3xx` meaning a misconfigured receiver, following one would deliver a
signed payload to a host the customer never registered — an SSRF primitive
handed to whoever controls the original URL.

### UUIDv7 for event IDs

The leading 48 bits are a Unix millisecond timestamp, so IDs sort in creation
order. Primary-key inserts append at the right edge of the B-tree instead of
scattering random pages, and the PK doubles as a rough time index.

**What it costs:** an event ID leaks its creation time. For a webhook event that
is not sensitive; for a user-facing resource ID it might be.

### Indexes are argued for, individually

This is a write-heavy path, so every index is amplification paid on each insert.
All four are justified inline in [`migrations/000001_init.up.sql`](migrations/000001_init.up.sql),
along with three indexes deliberately *not* created and why.

![index definitions and the query plan](docs/images/schema.png)

The partial index on `(endpoint_id, created_at) WHERE status='pending'` is the
interesting one. The equality column leads so `created_at` supplies the
`ORDER BY` — the plan above has no `Sort` node, so `LIMIT` short-circuits it.
Being partial means the index tracks the size of the *backlog* rather than
lifetime event volume: a delivered event drops out of it entirely.

### Valkey Streams over a LIST — and over Postgres SKIP LOCKED

Day 1 ships `queue.Queue` as an interface with no implementation, because
nothing consumes events yet and there is no behaviour to get wrong. The
reasoning is recorded in [`internal/queue/queue.go`](internal/queue/queue.go):

- **Not a LIST**, because a `LPOP` hands the job to a worker that might die
  holding it, and the job is then simply gone. That breaks at-least-once, which
  is the entire promise of this service. Streams keep a pending-entries list per
  consumer group, so an unacknowledged job can be reclaimed with `XAUTOCLAIM`.
- **Not Postgres `SELECT ... FOR UPDATE SKIP LOCKED`**, though it is genuinely
  tempting — it would drop Valkey entirely and make enqueue transactional. It
  loses because polling burns a connection per worker per poll, and it puts
  queue depth on the same disk as the durable event log, which is the last thing
  we want to saturate under load.

Day 6's benchmark is the honest test of that call. The interface is the seam
that makes reversing it cheap.

### Migrations are an operator action

`make migrate-up` runs the official `golang-migrate` image as a compose service.
Migrations deliberately do **not** run at application startup: several replicas
racing to migrate during a rolling deploy is a well-known way to deadlock one.

The `.sql` files are the single source of truth shared with the integration
tests, so the two paths cannot drift.

### Liveness and readiness are not the same probe

They answer different questions, and Kubernetes does different things with the
answers.

| | Question | Failure action |
|---|---|---|
| `/healthz` | Is this process broken beyond recovery? | Container is **killed and restarted** |
| `/readyz` | Should traffic go here *right now*? | Pod is **removed from the Service**, keeps running |

**What breaks if you conflate them.** Point liveness at the database, then give
the database a thirty-second blip. Every replica fails liveness simultaneously
and Kubernetes kills all of them at once. That:

1. **Drops every in-flight request and delivery**, turning a recoverable
   dependency blip into real data-movement failures.
2. **Starts a crash loop.** The restarted pods still cannot reach the database,
   fail again, and get killed again — now with exponential backoff on restarts,
   so recovery is delayed well past the database's own.
3. **Destroys the evidence.** The logs and in-memory state of the pods that saw
   the failure are gone.

The failure mode is worst precisely when the dependency recovers: the database
comes back and, instead of the fleet resuming, it is midway through a
`CrashLoopBackOff` cycle keeping it down for minutes longer.

The reverse conflation — readiness that checks nothing — is quieter but also
wrong: a pod that cannot reach Postgres stays in the Service and returns errors
that a healthy replica could have served.

So: **liveness checks only that the process is running its own event loop.**
**Readiness checks the dependencies this process needs to do useful work.**

The two binaries deliberately differ:

- **API readiness** includes a backlog check. Shedding ingest while the workers
  catch up beats accepting events we already know we cannot deliver on time.
- **Worker readiness** deliberately does **not**. A worker with a deep backlog is
  the thing working on it; pulling it from the Service would only slow recovery.

### Rate limiting and the breaker both fail *open*

If Valkey is unreachable, deliveries proceed. Both are protections for the
receiver and optimizations for us — refusing every delivery because a cache is
down would convert a degraded dependency into a total outage. The failure is
logged, never silent.

### Two Lua scripts, for the same reason

Both the token bucket and the breaker's half-open transition are
read-modify-write against a key that every worker goroutine in every replica is
racing.

- **Token bucket:** two workers can both read "1 token left" and both proceed, so
  an endpoint configured for 10/s quietly receives 20. `WATCH`/`MULTI`/`EXEC`
  would be correct but degrades into a retry storm under exactly the contention
  that makes a limiter matter.
- **Half-open probe:** when the cooldown expires, every worker checks at once. If
  they all read "cooldown elapsed" they all probe — delivering the precise
  stampede the breaker exists to prevent, at the worst possible moment. `SET NX`
  on a probe key makes the winner unique.

Time inside the bucket script comes from `redis.call('TIME')`, not from the
caller: a worker whose clock runs two seconds fast would credit itself two extra
seconds of tokens on every call.

### Tracing across the outbox needs durable context

This is the part that is easy to get wrong, and it is not solvable with
in-process propagation.

The outbox decouples ingest from enqueue on purpose: ingest writes a row and
returns; a background relay picks that row up later. So by construction the
relay has **no in-memory link** to the request that created the event — it is a
different goroutine, usually a different process, running minutes later.

Passing `context.Context` around cannot bridge that. The W3C trace carrier has
to be as durable as the event, so it lives in `events.trace_context`
([migration 000004](migrations/000004_event_trace_context.up.sql)). The relay
rehydrates it when claiming, `Enqueue` injects it into the stream entry as
ordinary string fields, and the worker extracts it before starting its span.

Without it every delivery opens its own unconnected trace: you can see that an
ingest happened and that a delivery happened, with no way to tell they were the
same event — which is exactly the question you are asking when something is
wrong.

---

---

## Running on Kubernetes

Nothing below is applied by hand. `make bootstrap` runs Terraform, Terraform
installs ArgoCD, and ArgoCD deploys the application from this repository.

```
deploy/k3d/cluster.yaml      the cluster            <- committed, not typed flags
terraform/                   everything in it       <- terraform apply
deploy/charts/webhook-relay  the application        <- deployed BY ArgoCD, not Terraform
gitops/                      what ArgoCD watches
```

### What this actually looks like

![the cluster after terraform apply](docs/images/cluster.png)

![migrations ran as a Helm hook before any pod started](docs/images/migrate.png)

The schema is at version 4 before a single application pod exists. That is the
`pre-upgrade` hook doing its job: new code never runs against an old schema.

### Self-healing

`selfHeal: true` means the repository is the desired state, continuously — not
just at deploy time.

![ArgoCD restoring a deleted Deployment](docs/images/selfheal.png)

Two seconds, and a different UID — genuinely recreated, not an incomplete
delete. This is also why every "fix it with kubectl" instruction in the
[runbook](docs/runbook.md) is marked temporary.

### Why Terraform stops at the cluster boundary

Terraform provisions infrastructure. ArgoCD deploys the application. The
obvious alternative — have Terraform install the app's chart too — is what most
people do, and it is worse in three specific ways:

| | Terraform deploys the app | ArgoCD deploys the app |
|---|---|---|
| Drift | Silent until the next apply, possibly weeks | Corrected in ~30s |
| Deploying a new image | Run Terraform from CI → **CI needs cluster credentials** | Commit a tag → CI needs nothing |
| Rollback | Revert and re-apply, with no record of what ran when | `argocd app rollback`, or `git revert` |

### Why pull-based delivery is more secure

This is the part worth being able to argue.

The push model — `kubectl apply` or `helm upgrade` from GitHub Actions —
requires a long-lived cluster credential stored in a third party's secret
store:

- **Any workflow in the repository can read it**, including one added by a pull
  request that modifies `.github/workflows`. That is a well-known escalation
  path.
- **The API server must accept inbound connections from GitHub's runner
  ranges**, so it is exposed to the internet rather than reachable only from
  inside the network.
- **A compromised runner has immediate, direct cluster access**, with no audit
  trail beyond logs the attacker can influence.

Pull inverts it. CI can only write to Git. ArgoCD, running *inside* the
cluster, reads from Git and applies:

- No cluster credential exists outside the cluster.
- The API server needs no inbound internet access at all.
- Every deployment is a commit — reviewable, attributable, revertible.
  `git log gitops/values.yaml` is the deployment history.
- A compromised runner can at worst commit a bad tag, which shows in the diff
  and is undone by `git revert`.

### The deployment path, end to end

```
merge to main
  → release.yml builds a multi-arch image
  → pushes to ghcr.io tagged sha-<12-char-sha>   (never :latest)
  → cosign signs it keylessly via the workflow's OIDC identity
  → Syft SBOM attached as a signed attestation
  → CI commits the new tag into gitops/values.yaml
  → ArgoCD notices the commit and syncs
  → Helm pre-upgrade hook runs migrations behind an advisory lock
  → new pods roll out
```

The image is never tagged `latest`. The chart **fails the render** if you try:

```
image.tag must be set to an immutable tag (a git SHA). 'latest' is refused:
it makes rollback impossible and lets two pods claiming one version run
different code.
```

### Signing and provenance

Cosign signs with **no key at all**. It exchanges the workflow's OIDC token for
a short-lived certificate from Fulcio and records the signature in the public
Rekor transparency log. There is no signing key to leak, and the certificate
binds the signature to *this workflow in this repository* — something a
checksum cannot express.

```bash
cosign verify \
  --certificate-identity-regexp '^https://github.com/singha105/webhook-relay/.github/workflows/release.yml@refs/heads/main$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/singha105/webhook-relay:sha-<sha>
```

The SBOM is attached as a signed attestation rather than an uploaded artifact,
so "what is in this image" is answerable from the image alone — which is what
matters at the moment a CVE lands and you need to know whether you are
affected.

### The SIGTERM ordering

Getting this wrong causes duplicate deliveries on every rollout, silently.

When a pod is deleted, **two things happen concurrently and Kubernetes does not
order them**: the kubelet sends `SIGTERM`, and the endpoints controller removes
the pod from the Service. The second is eventually consistent.

```
t=0     pod marked Terminating
        ├─ endpoint removal begins  (takes ~1-2s to propagate)
        └─ preStop sleep begins     ← holds the container open meanwhile
t=5s    preStop returns, SIGTERM delivered
t=5s    app stops claiming new work, finishes in-flight deliveries
t=?     app exits
t=45s   terminationGracePeriodSeconds expires → SIGKILL
```

Without the preStop delay, a process that exits promptly on `SIGTERM` is still
being routed to — the client sees connection refused, which on the ingest path
is a dropped event.

`terminationGracePeriodSeconds` is **computed**, not hand-set:
`preStopDelay + drainTimeout + 10s`. Hand-setting it is how you get a worker
SIGKILLed mid-delivery, leaving an unacked queue entry that is redelivered
later as a duplicate caused purely by a rollout.

The `preStop` uses the **native `sleep` action**, not `exec`. The image is
distroless — there is no shell and no `/bin/sleep`, so `exec: ["sh","-c","sleep 5"]`
would fail to start and the kubelet would go straight to `SIGTERM`, silently
removing the delay.

### Migrations, and the race they avoid

Migrations run as a Helm `pre-upgrade` hook, not an initContainer. An
initContainer runs once per pod — four replicas means four concurrent migration
attempts on every rollout.

The failure being prevented: two deploys overlapping (a rollback issued while a
rollout finishes, or ArgoCD retrying a failed sync) both read
`schema_migrations`, both see version N, both apply N+1. That is either a loud
duplicate-object error, or — worse — two `ALTER`s that both succeed and leave a
schema no migration file describes. golang-migrate takes a Postgres session
advisory lock, so the second process waits.

### NetworkPolicies

Default-deny, then enumerate. The asymmetry is the interesting part:

| | Postgres | Valkey | Internet |
|---|---|---|---|
| **API** | ✅ | ✅ | ❌ **denied** |
| **Worker** | ✅ | ✅ | ✅ except RFC1918 |

The API never makes an outbound call, so denying it egress means an SSRF in the
ingest path cannot become an outbound request — even though the API accepts
customer-supplied URLs at endpoint registration.

The worker *must* reach arbitrary addresses, so an allow-list is impossible.
What *is* possible is excluding `10/8`, `172.16/12`, `192.168/16` and
`169.254/16`, so a customer cannot register `http://10.0.0.1/` and use our
worker to scan the internal network. That closes the SSRF gap the Day 1 README
listed, at the network layer rather than with URL validation an attacker can
encode around.

> **Not enforced on this cluster.** k3s ships Flannel, which accepts
> NetworkPolicy objects and silently ignores them. Real enforcement needs
> Calico or Cilium. The policies are correct and committed; they are currently
> documentation.

### Secrets in Git

See **[docs/secrets.md](docs/secrets.md)** for the full argument. The summary:

- **Sealed Secrets** for anything ArgoCD deploys — a `SealedSecret` is a plain
  manifest, so ArgoCD needs no plugin, no sidecar, and no key.
- **SOPS + age** for values a human or a CI job needs, which a SealedSecret can
  never provide (it can only be decrypted by the controller).
- **Best of all: generate at provision time.** The Postgres and Grafana
  passwords are generated by Terraform and never enter Git in any form. That is
  strictly better than encrypting and committing, and it is available whenever
  the thing creating the cluster can also create the secret.

The trap: the Sealed Secrets private key lives in the cluster. Recreate the
cluster and every committed `SealedSecret` becomes undecryptable — ArgoCD syncs
green and no `Secret` ever appears. `make seal-backup` exports it.

### Resource requirements

| Profile | Docker memory | What you get |
|---|---|---|
| `full` | **~6 GiB** | 1 server + 2 agents, 3-replica Postgres with real failover, Loki, Chaos Mesh |
| `lowmem` | ~3.5 GiB | 1 server + 1 agent, single Postgres, no Loki, no Chaos Mesh, 6h metrics retention |

```bash
make cluster-up PROFILE=lowmem && make bootstrap PROFILE=lowmem
```

The full profile needs Docker Desktop → Settings → Resources → Memory ≥ 6 GiB.

---

---

## Service level objective

> **99% of events are delivered within 30 seconds of ingest, measured over a
> rolling 7 days.**

Three deliberate choices in that sentence:

- **From ingest, not from dequeue.** The customer's clock starts when they hand
  us the event. Measuring from when *we* got around to scheduling it would let
  us hide a backlog by not looking at it.
- **Delivered, not attempted.** A 5xx that is retried and eventually succeeds
  inside 30 seconds met the objective. The customer does not care how many
  attempts it took.
- **7 days, not 30.** Long enough that one bad afternoon does not dominate,
  short enough that the budget resets while the incident is still remembered.

### Measuring it

The SLI is the fraction of delivery attempts that succeeded, read from the
histogram's cumulative buckets rather than from a quantile — a quantile answers
"what is the value at rank p", which cannot tell you "what fraction met the
target".

```promql
# SLI: proportion of successful deliveries over the SLO window.
sum(rate(webhook_delivery_attempts_total{status_class="2xx"}[7d]))
/
sum(rate(webhook_delivery_attempts_total[7d]))
```

That covers "delivered". The "within 30 seconds" half is the backlog gauge,
because end-to-end latency is exactly how long the oldest undelivered event has
been waiting:

```promql
# Fraction of time the backlog stayed inside the 30s objective.
avg_over_time(
  (max(webhook_queue_oldest_message_age_seconds) < bool 30)[7d:1m]
)
```

Both are precomputed as [recording rules](deploy/observability/rules/webhook-relay.yml).
A panel and an alert that compute the SLO with slightly different expressions
will eventually disagree, and at that point nobody trusts either.

### The error budget

A 99% objective over 7 days permits **1% of events to miss the target**.

| | |
|---|---|
| Window | 7 days = 10,080 minutes |
| Objective | 99% |
| Error budget | 1% = **100.8 minutes** of total unavailability |
| At 100 events/s | 1% of ~60.5M events = **~604,800 events** may miss |

Budget *consumed* so far in the window:

```promql
# 0 = untouched, 1 = fully spent, >1 = objective missed.
(
  1 - (
    sum(rate(webhook_delivery_attempts_total{status_class="2xx"}[7d]))
    /
    sum(rate(webhook_delivery_attempts_total[7d]))
  )
) / 0.01
```

### Burn rate, and why the alert uses two windows

Burn rate is how fast the budget is being spent relative to sustainable. A burn
rate of 1 exhausts the budget exactly at the end of the window; a burn rate of
14.4 exhausts a 30-day budget in about two days.

```promql
# Fast burn: 14.4x sustainable, confirmed over both a short and a long window.
(
  (1 - webhook:delivery_success:ratio_rate5m) > (14.4 * 0.01)
  and
  (1 - webhook:delivery_success:ratio_rate1h) > (14.4 * 0.01)
)
```

The `and` across a 5-minute and a 1-hour window is the point. The short window
alone pages on a single bad minute; the long window alone takes an hour to
notice a total outage. Requiring both means the alert is **fast to fire and slow
to flap** — it needs the failure to be both sudden and sustained.

### What the budget is *for*

An error budget that is never spent means the objective is too loose, or too
much engineering is going into reliability that customers cannot perceive.
Budget remaining is permission to ship; budget exhausted is a signal to stop
shipping features and fix delivery. That is the entire reason to state a number
rather than "we try hard".

---

---

## Testing

![make test and make lint](docs/images/test.png)

Integration tests run against **real Postgres in a container** via
testcontainers-go. There are no database mocks, because most of what is worth
testing in the store layer *is* database behaviour — `CHECK` constraints,
`ON CONFLICT` arbitration, cascade deletes, and transaction visibility. A mock
would only assert that our fake behaves like our fake.

One container is shared per test binary; each test gets its own schema, so tests
run in parallel without seeing each other's rows.

### The pipeline is tested as a pipeline

Twelve integration tests in [`internal/worker`](internal/worker) run a complete
system — real Postgres, real Valkey, a real HTTP receiver, the relay, and the
worker pool, wired exactly as `cmd/worker` wires them. They cover success, retry
then success, permanent `4xx`, exhaustion into the DLQ, replay from the DLQ,
signature verification, header correctness, inactive endpoints, and the
consecutive-failure counter.

They earned their keep immediately by catching a real bug: `ReplayEvent` checked
for the pgx no-rows sentinel, but `scanEvent` had already normalized it to
`models.ErrNotFound` — so the check never matched, `ErrNotReplayable` was
unreachable, and the API would have answered `500` where it should answer `409`.

### The race test was verified by mutation

A concurrency test that passes proves nothing unless it can fail. Swapping the
`ON CONFLICT` insert for the naive `SELECT`-then-`INSERT` makes it fail exactly
as it should:

```
--- FAIL: TestCreateEventIdempotencyRace (1.52s)
    goroutine 0: CreateEvent() = create event: ERROR: duplicate key value
    violates unique constraint "events_endpoint_idempotency_key_uniq"
    (SQLSTATE 23505) (a lost race must not surface as an error)
```

Getting there required fixing the test itself. `pgxpool` opens connections
lazily, so the first goroutine off the line completed its whole transaction
while the others were still finishing TCP handshakes — they never overlapped,
and the test passed the broken implementation too. Both race tests now pre-warm
their connection pools before the starting gate opens.

---

---

## Layout

```
deploy/k3d/         cluster definition — committed, not typed flags
terraform/          cluster infrastructure: CNPG, Valkey, observability, ArgoCD
deploy/charts/      the Helm chart ArgoCD deploys
gitops/             app-of-apps; gitops/values.yaml is written by CI
docs/runbook.md     on-call procedures
docs/secrets.md     how secrets live in Git without being readable

cmd/api/            HTTP ingest + management API
cmd/worker/         outbox relay + delivery worker pool
internal/models/    domain types and validation; no I/O
internal/store/     Postgres access; owns every SQL statement
internal/queue/     Valkey Streams work queue
internal/relay/     transactional outbox: Postgres -> queue
internal/worker/    delivery pool, retry and DLQ decisions
internal/delivery/  HTTP client, backoff, classification, dedup
internal/httpapi/   routing, middleware, handlers
internal/config/    environment-only configuration
pkg/webhook/        PUBLIC: signature verification for receivers
migrations/         golang-migrate SQL, indexes justified inline
deploy/compose/     local stack: postgres 16, valkey 8, api, worker
test/               testcontainers helpers and the full-pipeline harness
test/sink/          controllable webhook receiver for tests and the demo
```

---

---

## How it was built

Six days, one thing at a time, each merged to `main` behind CI and tagged.

| Day | Deliverable |
|---|---|
| **1** | Ingest API, schema, idempotency, local stack, CI |
| **2** | Outbox relay, Valkey Streams, delivery worker, HMAC signing, full-jitter retries, DLQ, replay |
| **3** | Rate limiting, circuit breaker, tracing, metrics, Grafana, graceful shutdown, SLO |
| **4** | k3d + Terraform + Helm + ArgoCD, signed images, NetworkPolicies, alerts, runbook |
| **5** | k6 load tests, the bottleneck hunt and its fix, chaos experiments, the postmortem |
| **6** | ADRs, `make demo`, the front door you are reading |

## License

MIT — see [LICENSE](LICENSE).
