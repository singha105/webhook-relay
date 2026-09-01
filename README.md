# webhook-relay

Reliable webhook delivery with at-least-once semantics, retries, and chaos-tested
failure modes — the kind of service Stripe or Svix runs internally.

[![CI](https://github.com/singha105/webhook-relay/actions/workflows/ci.yml/badge.svg)](https://github.com/singha105/webhook-relay/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

---

## Status: Day 2 of 6

This repository is being built in public, one day at a time. **Only what is
described below actually works.** Nothing here is a stub presented as finished.

| | |
|---|---|
| ✅ **Working today** | Ingest API, idempotent ingest, transactional outbox, Valkey Streams queue, delivery worker pool, HMAC-SHA256 signing, full-jitter retries, dead letter queue, replay, stale-claim recovery |
| ⬜ **Not built yet** | Circuit breaker (Day 3), observability (Day 4), Kubernetes and chaos testing (Day 5), load test and postmortem (Day 6). `rate_limit_per_sec` is stored and validated but **not enforced**. |

The roadmap is at the [bottom of this file](#roadmap).

---

## Quick start

Requires only **Docker** and **make**. Everything else runs in containers.

```bash
git clone https://github.com/singha105/webhook-relay.git
cd webhook-relay
make up && make migrate-up
```

![make up and make migrate-up](docs/images/up.png)

`make up` refuses to start if a host port is already taken, and tells you which
one and how to override it — ports live in `.env` (see `.env.example`). That is
deliberate: on the machine this was built on, 8080 belonged to Apache and 5432
to a local Postgres, and a bind error from inside Docker is a bad way to learn
that.

Then seed some data:

```bash
make seed
```

To watch delivery actually happen — retries, dead-lettering, replay, and a
signature verified against `openssl` — start the stack with a controllable
receiver and run the demonstration:

```bash
make demo-up && make demo
```

<details>
<summary><code>make</code> targets</summary>

| Target | What it does |
|---|---|
| `up` / `down` | Start / tear down the stack (`down` also deletes volumes) |
| `migrate-up` / `migrate-down` | Apply / roll back migrations |
| `seed` | Register a demo endpoint and post a few events |
| `test` / `test-short` | Full suite (needs Docker) / unit tests only |
| `lint` / `fmt` / `vet` | golangci-lint, formatting, vet |
| `verify` | Everything CI runs |
| `scan` | Scan the whole git history for secrets |

</details>

---

## What it does today

### Register an endpoint

The signing secret is generated server-side and returned **exactly once**. It is
never included in any subsequent response.

![registering an endpoint](docs/images/endpoint.png)

The secret is stored in plaintext rather than hashed, because the delivery
worker has to recompute an HMAC over the payload at send time. Unlike a
password, it cannot be a one-way hash. "Shown once" is an API-surface guarantee,
enforced by the response shape and asserted by tests — not a storage guarantee.

### Ingest an event

Ingest does four things and no more: decode, validate, write one row, return.
No delivery work, no queue publish, no pre-flight lookup on the request path.

![posting an event](docs/images/ingest.png)

`202`, not `201` — the event is durably recorded, but nothing has been
delivered. Claiming `201 Created` would imply the work is done.

### Idempotent ingest

Replay the same `Idempotency-Key` and you get the original event back with a
`200` instead of a `202`. Ten concurrent identical requests produce exactly one
row.

![idempotent ingest](docs/images/idem.png)

This is resolved by the database, not the application. See
[the idempotency race](#the-idempotency-race) below.

### Deliver it, retry it, dead-letter it

A worker signs the payload, POSTs it, and records every attempt. When the
endpoint keeps failing, retries back off with full jitter until the attempt
budget is spent and the event is dead-lettered.

![retry with full jitter, then dead letter](docs/images/retry.png)

Look at the gaps rather than the timestamps. The **ceiling** doubles — 2s, 4s,
8s, 16s, 32s — but each actual delay is a uniform draw from `[0, ceiling)`, so
the sequence trends upward without being monotonic. The 5→6 gap of 10s under a
32s ceiling is the jitter working, not a bug. A perfectly doubling sequence
would mean it was not.

Note also `duplicate dispatches: {}` — six attempts, six deliveries, no
attempt sent twice.

### Replay it, and verify the signature

Point the endpoint at something healthy and replay from the DLQ. The attempt
budget resets; the failure history does not disappear.

![replay from the DLQ and verify the signature](docs/images/replay.png)

The last step recomputes the HMAC with `openssl` and compares it to the `v1=`
digest we sent. It matches, which means the signature scheme is verifiable by
anything that can compute an HMAC — not just by our own Go code.

### Everything self-probes

![container health](docs/images/health.png)

The runtime image is distroless — no shell, no curl — so each binary probes
itself via `-healthcheck`. The API GETs its own `/readyz`; the worker, which
serves no HTTP, checks that Postgres and Valkey are both reachable using its
real configuration.

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

`/healthz` touches no dependency. A liveness probe that checked Postgres would
restart every API pod during a database blip, turning a recoverable outage into
a cluster-wide crash loop that also drops in-flight requests. `/readyz` pings
Postgres and pulls the pod out of the Service without killing it.

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

## Layout

```
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

## Roadmap

| Day | Deliverable |
|---|---|
| **1** ✅ | Ingest API, schema, idempotency, local stack, CI |
| **2** ✅ | Outbox relay, Valkey Streams, delivery worker, HMAC signing, full-jitter retries, DLQ, replay |
| **3** | Per-endpoint circuit breaker and rate limiting |
| **4** | OpenTelemetry → Prometheus, Tempo, Loki, Grafana |
| **5** | k3d + Helm + Terraform + ArgoCD; Chaos Mesh fault injection |
| **6** | k6 load test, benchmark numbers, and the postmortem |

`endpoints.consecutive_failures` is already maintained by the worker — reset on
success, incremented on failure — so Day 3's circuit breaker reads a counter
that is already correct rather than starting from zero against live data.

---

## Known gaps

Stated plainly rather than discovered by a reviewer:

- **No authentication.** Anyone who can reach the API can register endpoints and
  post events. Real deployments need API keys per tenant.
- **No SSRF protection.** Endpoint URLs may point at loopback and private
  ranges, which is required for local testing but would need an egress allowlist
  in a hosted deployment.
- **No rate limiting yet.** `rate_limit_per_sec` is stored, validated, and read
  by the worker, but nothing throttles on it. A busy endpoint can be hit as fast
  as the pool can dispatch. Day 3.
- **No circuit breaker yet.** `consecutive_failures` is maintained correctly but
  nothing reads it to stop delivering. An endpoint that has been down for an
  hour still gets the full retry schedule for every event. Day 3.
- **Delivery is at-least-once, deliberately.** The dedup guard narrows the
  duplicate window; it does not eliminate it. Receivers must deduplicate on
  `X-Webhook-Id`.
- **Offset pagination** on `GET /v1/endpoints` drifts under concurrent inserts.
  Acceptable because endpoints are registered by humans, not by traffic. The
  events table would need keyset pagination.

## License

[MIT](LICENSE)
