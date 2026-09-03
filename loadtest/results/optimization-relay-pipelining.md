# Optimization: pipelined relay enqueue

Raw before/after for the one code change Day 5 made on the basis of a
measurement. Every number here came from `loadtest/drain-throughput.sh`,
3 repeats, 3000-6000 events, on the hardware in [../README.md](../README.md).

## How the bottleneck was found

Not by guessing. By eliminating, in this order:

| Hypothesis | Test | Result |
|---|---|---|
| pgx pool too small | `DB_MAX_CONNS` 10 → 25 | **no effect** |
| worker concurrency too low | `WORKER_CONCURRENCY` 10 → 50 | **no effect** |
| both, since they gate each other | 50 + pool 60, together | **no effect** (750 vs 769) |
| a saturated resource | `docker stats` during a drain | **nothing saturated** |

The `docker stats` result is what redirected the search. At full throughput:

```
webhook-relay-worker-1       123.7%   (of 800% available)
webhook-relay-postgres-1     123.6%
webhook-relay-valkey-1        32.6%
webhook-relay-sink-1          11.7%
```

Nothing is CPU-bound. The system is **latency-bound** — waiting on sequential
round trips, not computing. That rules out "add more workers" as a fix and
points at the number of round trips instead.

Reading the relay with that in mind: it is the sole queue producer for the
whole system, and `relayOnce` called `Enqueue` once per event in a serial
`for` loop. One `XADD` round trip per event, from a single goroutine —
**O(n) round trips per batch**.

## The change

`EnqueueBatch` pipelines the XADDs: a 100-event batch costs one round trip
instead of 100. Pipelining is not a transaction, so a partial failure is
possible; the method returns a count and the relay releases the claims on the
remainder rather than assuming all-or-nothing.

Per-event trace context is preserved — each item carries its own `Ctx` and is
injected separately, so batching does not collapse many ingest traces into one.
That is asserted by a test, not assumed.

## Results

| Configuration | Run 1 | Run 2 | Run 3 | Median | vs baseline |
|---|---|---|---|---|---|
| Baseline (serial enqueue, 10/10) | 810.8 | 769.2 | 769.2 | **769.2** | — |
| Pipelined enqueue (10/10) | 810.8 | 857.1 | 789.5 | **810.8** | **+5.4%** |
| Pipelined + concurrency 50, pool 60 | 895.5 | 869.6 | 845.1 | **869.6** | **+13.1%** |

Units: events/sec sustained delivery.

## What this is, and what it is not

It **is** a real fix: the serial loop was O(n) round trips in the one component
every event must pass through. That ceiling would harden as batch size grows or
as the relay moves further from Valkey — on this machine both are ~0.3ms apart
on a Docker bridge, which is the friendliest possible case.

It is **not** the 5× the setup implies. +5.4% alone, +13.1% combined. An honest
reading is that the relay was *a* serialization point but not *the* dominant
one.

## The part I did not solve

Raising delivery concurrency 5× (10 → 50) buys 7%. With nothing CPU-saturated
and every resource idle, 5× the parallelism should buy far more than 7%. That
gap means a serialization point remains that I did not isolate within the day.

Candidates not yet eliminated, in the order I would test them next:

1. **Round trips per delivery.** Each one makes ~12 sequential calls
   (LoadForDelivery, breaker, rate limiter, NextAttemptNumber, dedup claim,
   HTTP, MarkDelivered's multi-statement transaction, ack). At 10 concurrent
   deliveries and ~12ms each, ~830/s falls out exactly — the arithmetic fits
   the observed number well enough to be worth chasing first.
2. **`NextAttemptNumber` as a separate query**, foldable into `LoadForDelivery`
   as a LATERAL subquery. Saves one round trip of the twelve — ~1ms on a 12ms
   delivery. Real, but not the answer on its own.
3. **`ClaimDueEvents` write amplification** — ten `UPDATE … RETURNING`
   transactions per relay tick, each an fsync.

I am recording this rather than shipping a tidier story, because the measurement
does not support one. The eliminations above are as much of the result as the
improvement is: "pool size and worker count are not the bottleneck" is a real
finding about this system, and it was the opposite of my first guess.
