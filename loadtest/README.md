# Load testing

Every number in this directory comes from a run reproducible with a committed
script. Nothing here is estimated, extrapolated, or copied from a benchmark of
something else.

## The hardware, stated up front

| | |
|---|---|
| Machine | Apple M3, 8 cores, 8 GB RAM |
| Docker | 3.825 GiB allocated, 8 CPUs |
| Stack | docker-compose: Postgres 16, Valkey 8, api, worker, sink |
| Load generator | k6 v2.2.0, **on the same machine** |

**These numbers reflect a laptop, not a deployment.** The load generator,
Postgres, Valkey, the API, the worker, and the receiver are all competing for
the same 8 cores and the same 3.8 GiB. A defensible small number measured
honestly is more useful than a large one produced by a benchmark that did not
measure what it claimed.

The most important consequence: **k6 itself is a significant consumer.** At the
top of the ramp k6 is running 2000 VUs on the same box as everything it is
measuring, so some of the observed latency is contention with the load
generator rather than with the service.

## Running them

```bash
make up && make migrate-up
docker compose -f deploy/compose/docker-compose.yml --env-file .env --profile demo up -d sink

k6 run loadtest/smoke.js       # 1 VU, 1 min      — is it working at all
k6 run loadtest/baseline.js    # 200/s, 5 min     — the reference number
k6 run loadtest/ramp.js        # 0-5000/s, 10 min — where does it break
k6 run loadtest/soak.js        # 500/s, 30 min    — does it drift

./loadtest/drain-throughput.sh # delivery throughput, isolated
```

## Results

### Ingest

| Scenario | Offered | Achieved | p50 | p95 | Errors |
|---|---|---|---|---|---|
| smoke | 2/s | 1.98/s | 6.34 ms | 12.51 ms | 0 |
| baseline | 200/s | 199.95/s | **0.68 ms** | **2.30 ms** | **0** |
| ramp (peak) | 5000/s | **623/s** | 2188 ms | 4402 ms | **0** |

Raw JSON in [`results/`](results/).

### What the ramp found

The knee is around **620 events/sec** on this hardware. Past it the system does
not fail — it queues:

```
ingest=321    delivery=151    depth=41445    age=75s
ingest=604    delivery=247    depth=53993    age=106s
ingest=684    delivery=282    depth=84218    age=152s
ingest=875    delivery=331    depth=100018   age=196s
```

Three things worth noticing:

1. **Zero errors at any load.** Ingest latency degraded from 2.30 ms to 2188 ms
   p50, but not one request failed. The system sheds nothing; it absorbs. That
   is the intended behaviour — a dropped POST is an event the customer must
   retry, so the design prefers a slow accept to a fast reject.

2. **Delivery, not ingest, is the constraint.** Ingest sustained 875/s while
   delivery plateaued near 330/s. Everything downstream of the ingest INSERT is
   where the capacity limit lives.

3. **Queue depth pinned at exactly 100,018.** That is the stream's
   `MAXLEN ~ 100000`. Under sustained overload the stream is trimming entries
   that have not been consumed. Events are not lost — Postgres is the system of
   record and the outbox relay re-enqueues anything whose lease expires — but
   the queue silently stops being the complete picture, and `webhook_queue_depth`
   stops being a truthful backlog metric above that bound. `OldestBacklogAge`
   reads from Postgres precisely so it stays truthful when this happens.

## The bottleneck hunt

The candidates, and what measurement said about each:

| Candidate | Verdict | Evidence |
|---|---|---|
| Rate limiter | **Ruled out** | `webhook_rate_limited_total` never produced a series — it never fired |
| Postgres CPU | **Not saturated** | Peaked at 147% of 800% available (~18% of the box) |
| pgx pool size | **See below** | Measured, and the first result was wrong |
| Postgres max_connections | **Ruled out** | 21 backends against `max_connections=100` |

### Where the first answer was wrong

`pg_stat_activity` sampling showed the ingest `INSERT` dominating active
queries, and connection sampling showed up to 21 backends all active at once
against pools of 10 + 10. That looked conclusive: the pool is the bottleneck.

It was not. Raising `DB_MAX_CONNS` from 10 to 25 produced 452/s and 588/s
against a baseline of 529.8/s and 522.9/s — no difference, just noise. The
hypothesis was wrong and the measurement said so.

Worse, the *first* attempt at that experiment was invalid for a different
reason: `DB_MAX_CONNS` was not plumbed through `docker-compose.yml`, so setting
it on the command line changed nothing at all. The 378/s it produced was a
measurement of the same configuration twice.

Both mistakes are recorded here rather than quietly deleted, because "I formed
a hypothesis from a plausible signal and the data refused it" is the part of
performance work that is usually edited out.

There was a third instance of the same class of error, worth naming because it
is a trap specific to this setup: a later run appeared to show that raising
concurrency and the pool *together* also did nothing. It had not been raised at
all. `COMPOSE="docker compose -f ..."` followed by `$COMPOSE up -d` silently
does nothing useful in zsh, which — unlike bash — does not word-split unquoted
variables, so the whole string was treated as one command name. The worker's
own startup log (`"concurrency":10, "db_max_conns":10`) is what caught it.

**Every configuration claim below was verified against the worker's startup log
before its numbers were recorded.** Three invalid experiments in one day is
enough to make that a standing rule rather than a good intention.

### What the elimination actually found

| Candidate | Verdict |
|---|---|
| pgx pool size alone (10 → 25) | no effect |
| worker concurrency alone (10 → 50) | no effect |
| both together (50 / pool 60) | no effect — 750 vs 769 |
| any saturated resource | **none** |

`docker stats` during a drain, with 800% available:

```
webhook-relay-worker-1       123.7%
webhook-relay-postgres-1     123.6%
webhook-relay-valkey-1        32.6%
webhook-relay-sink-1          11.7%
```

Nothing is CPU-bound. The system is **latency-bound** — waiting on sequential
round trips, not short of capacity. That is what redirected the search from
"add more workers" to "make fewer round trips", and led to the one code change
of the day: the relay was the sole producer for the whole system and enqueued
one event per round trip in a serial loop.

Full before/after, including the part that is still unexplained:
[results/optimization-relay-pipelining.md](results/optimization-relay-pipelining.md).

| Configuration | Median |
|---|---|
| Baseline (serial enqueue) | 769.2 events/sec |
| Pipelined enqueue | **810.8 events/sec** (+5.4%) |
| Pipelined + concurrency 50 / pool 60 | **869.6 events/sec** (+13.1%) |

The honest caveat: 5× the delivery concurrency still buys only 7% on an
unsaturated machine, so a serialization point remains that this day did not
isolate. Ranked candidates are in the linked writeup.
