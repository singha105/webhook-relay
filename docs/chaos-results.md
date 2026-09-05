# Chaos experiment results

Ten experiments. For each: what was predicted **before** running it, what
actually happened, and — where they differ — why the prediction was wrong.

Predictions are committed in the manifests themselves
([`chaos/`](../chaos/)), written before the runs, so they cannot be
retrofitted to the results.

## Where these ran

| | |
|---|---|
| Machine | Apple M3, 8 cores, 8 GB RAM |
| Docker | 3.825 GiB allocated |
| Environment | docker-compose stack (Postgres 16, Valkey 8, api, worker, sink) |

**The Kubernetes cluster could not host these.** The full profile needs about
6 GiB and Docker has 3.825, so Chaos Mesh was never installed. Experiments that
require it are marked NOT RUN below, with their manifests committed and their
predictions unresolved — a prediction with no result is honest; a result
invented to match one is not.

The experiments that DID run used the Compose stack, driven by the scripts in
[`chaos/compose/`](../chaos/compose/). Those exercise the same binaries, the
same Postgres, and the same Valkey; what they cannot exercise is the
Kubernetes-specific behaviour — pod rescheduling, NetworkPolicy enforcement,
CloudNativePG failover.

---

## Summary

| # | Experiment | Status | Prediction |
|---|---|---|---|
| 1 | Kill a worker mid-delivery | Covered by #2 | — |
| 2 | Same, dedup disabled | **RUN** | **Correct** |
| 3 | Poison message | **RUN** | **Correct** |
| 4 | 30s latency vs 10s timeout | NOT RUN | needs Chaos Mesh |
| 5 | Kill Valkey | Script committed | needs a run |
| 6 | Kill the Postgres primary | NOT RUN | needs CNPG, 3 replicas |
| 7 | Workers to zero for 5 min | NOT RUN | needs Chaos Mesh |
| 8 | Partition worker from Valkey | NOT RUN | needs Chaos Mesh |
| 9 | CPU stress on a worker | NOT RUN | needs Chaos Mesh |
| 10 | Receiver at the timeout boundary | **RUN** | **Wrong** — see below |
| 11 | What happens to a suppressed event | **RUN** | **Correct** — the guard defers, not prevents |

---

## Experiment 2 — duplicate deliveries

**This is the postmortem.** Full writeup:
[postmortem-duplicate-delivery.md](postmortem-duplicate-delivery.md).

### Prediction

With the delivery-side dedup guard disabled, killing a worker between "HTTP
request sent" and "queue entry acknowledged" produces duplicate deliveries: the
stale-claim reclaim hands the entry to another worker, which re-sends the same
attempt. Re-enabling the guard should give zero.

### Method

300 events, sink holding each request open for 3s, `kill -9` on the worker
mid-flight, then wait out the 60s stale-claim reclaim. Identical run twice, the
only difference being `DELIVERY_DEDUP_ENABLED`.

```
./chaos/compose/02-duplicate-delivery.sh
```

### Result — prediction correct

```
RUN 1 of 2 - dedup DISABLED
  events posted:                 300
  deliveries the sink received:  300
  distinct events at the sink:   292
  DUPLICATE (event, attempt):    8

  01a0683c-ccb9-74ae-b046-cec48225323d:1 delivered 2 times
  01a0683c-ccd7-7d18-b0d3-dd1571d6c2c1:1 delivered 2 times
  01a0683c-ccdb-7691-afe7-6cc345ac4b4e:1 delivered 2 times
  01a0683c-cce6-7e39-b3c2-978520639331:1 delivered 2 times
  01a0683c-ccea-7779-ae53-57750a45cc53:1 delivered 2 times
  01a0683c-ccf2-7235-a12f-de2790be650c:1 delivered 2 times
  01a0683c-cd29-78ae-9b18-c898804137a8:1 delivered 2 times
  01a0683c-cd2f-7646-9701-24f925018719:1 delivered 2 times

RUN 2 of 2 - dedup ENABLED
  events posted:                 300
  deliveries the sink received:  300
  distinct events at the sink:   300
  DUPLICATE (event, attempt):    0
```

Note the shape of run 1: **300 deliveries but only 292 distinct events.** Eight
events were delivered twice and eight were not delivered at the moment of
counting — the receiver did 300 units of work for 292 events. Every one of
those 8 is a double charge, a double shipment, or a double email at a receiver
that is not idempotent.

Raw output: [`chaos/results/02-duplicate-delivery.txt`](../chaos/results/02-duplicate-delivery.txt).

### What this settles

The guard is load-bearing. It is now the subject of a committed, repeatable
experiment that fails if it regresses — which is the only thing that
distinguishes a working safety control from one nobody has ever tested.

---

## Experiment 3 — poison message

### Prediction

No single message can wedge a worker. The worker never *parses* the payload —
the API stores it as JSONB and the worker forwards opaque bytes — so there is
no user-controlled code path in the delivery loop that can panic on content.

### Result — prediction correct

```
  deeply nested object         -> HTTP 202
  float at max double          -> HTTP 202
  denormal float               -> HTTP 202
  deeply nested arrays         -> HTTP 202
  empty object                 -> HTTP 202
  top-level array              -> HTTP 202
  top-level scalar             -> HTTP 202
  top-level null               -> HTTP 202
  duplicate keys               -> HTTP 202
  3KB string                   -> HTTP 202
  malformed JSON               -> HTTP 400

  event states:  delivered: 10
  worker restarts since start: 0
  worker state: running
```

Ten of eleven accepted and delivered; malformed JSON rejected at ingest with a
400, never reaching a worker. Zero restarts, no head-of-line blocking.

### An incidental finding

`payload: null` is accepted with a 202. It is valid JSON and passes validation,
so it is stored as a JSONB null and delivered as the four bytes `null`. That is
defensible — the payload is opaque to us by design — but it is not obviously
intentional, and a receiver expecting an object will get something it may not
handle. Filed as a question rather than a bug; see Known gaps.

---

## Experiment 10 — a receiver at the timeout boundary

### Prediction

A receiver answering 200 after 9.9s against a 10s client timeout produces
double-*processing*, not duplicate dispatch. We time out, classify it retryable
with a nil status code, and send the NEXT attempt — a different attempt number,
so the dedup guard correctly does not suppress it. The receiver, which already
succeeded, handles the event again.

Expected evidence: the sink records more deliveries than there are events,
while our own records show each event delivered once.

### Result, part 1 — the prediction was WRONG

```
  sink responds 200 after 9.9s; client timeout is 10s
  events posted:                    30
  our records: delivered            30
  our records: total attempts       30
  our records: timeouts (null code) 0
  the RECEIVER actually processed   30   (distinct events: 30)
  double-processed by the receiver: 0
```

Zero timeouts. Zero duplicates. Thirty events, thirty attempts, thirty
deliveries — a perfectly clean run.

**Why the prediction was wrong:** I predicted an effect that requires the
boundary to actually be crossed, then set up a case that never crosses it. A
deterministic 9.9s sink delay against a 10s timeout has 100ms of headroom, and
nothing in the path — a local Docker bridge, an idle sink — consumes 100ms of
jitter. I had reasoned about the *mechanism* correctly and then chosen a
parameter that never triggers it.

This is the useful kind of wrong. "9.9 is close to 10" is intuition; whether
the gap is larger than the variance is an empirical question, and here it was.

### Result, part 2 — crossing the boundary for real

Re-run with the delay at 10.4s, which does cross:

```
  sink responds 200 after 10.4s; client timeout is 10s
  events posted:                    10
  our records: delivered            0
  our records: dead-lettered        0
  our records: total attempts       15
  our records: timeouts (null code) 15
  the RECEIVER actually processed   15   (distinct events: 8)
  double-processed by the receiver: 5
```

Now the mechanism shows itself, and it is worse than predicted:

**The receiver successfully processed 15 requests. Our system recorded zero
successes.** Every attempt timed out at 10s while the sink went on to complete
at 10.4s and record a delivery we never learned about. Five events were
processed twice; had the run continued to exhaust all 6 attempts, each event
would have been processed up to 6 times and then dead-lettered as a *failure*.

The dedup guard from experiment 2 correctly does not fire here — each retry is
a legitimately new attempt number, not a duplicate dispatch of the same one.
This is the boundary of what sender-side dedup can do, and it is exactly the
case called out in the
[postmortem](postmortem-duplicate-delivery.md): only receiver-side
idempotency on `X-Webhook-Id` closes it.

Raw output:
[`chaos/results/10-timeout-boundary-under.txt`](../chaos/results/10-timeout-boundary-under.txt),
[`chaos/results/10-timeout-boundary-over.txt`](../chaos/results/10-timeout-boundary-over.txt).

### The real lesson

A timeout is not a failure — it is an **unknown**. We record it as a failure
because that is the only safe assumption available to us, and that assumption
is wrong exactly when the receiver is slowest, which is when it can least
afford duplicate work. A receiver near the timeout boundary gets the worst
behaviour our system produces.

---

## Not run, and why

Experiments 4, 6, 7, 8 and 9 need infrastructure this machine could not host:

- **4, 7, 8, 9** need Chaos Mesh, which needs the cluster, which needs ~6 GiB.
- **6** needs CloudNativePG with 3 replicas; the low-memory profile runs 1, and
  a single instance has nothing to fail over to.

Their manifests are committed with predictions intact. Running them is a
`docker` memory setting and a `make bootstrap` away — the code is not the
blocker.

Recording them as unresolved is the point. An experiment log where every
prediction was confirmed is a log where nothing was learned, and a log with
invented results is worse than no log.


---

## Experiment 11 — what happens to an event the guard suppresses

Added on Day 6, because `make demo` surfaced something experiment 2 could not
see: after a hard kill, ten events sat in `delivering` with no recorded
outcome and stayed there.

### Prediction

The guard does not prevent the duplicate, it **delays** it. The event cycles —
lease expires, relay re-enqueues, guard suppresses, worker acks, stranded
again — until the dedup marker's TTL expires, and is then delivered a second
time. So over a window longer than `DELIVERY_DEDUP_TTL`, duplicates > 0.

**What would falsify it:** the event reaching a terminal state without a
second delivery, or staying stranded forever with no duplicate appearing.

### Method

Timings compressed so the full cycle is observable in minutes rather than the
fifteen the defaults imply — dedup TTL 60s, stale claim 20s, lease 45s. The
mechanism is unchanged; only the clock is.

The script **asserts the compressed config actually applied** before measuring.
The first attempt silently used the defaults, because `DELIVERY_DEDUP_TTL` was
not plumbed through `docker-compose.yml` — the same class of error that
invalidated three Day 5 experiments. That is now the fourth, and the assertion
exists so there is not a fifth.

```
./chaos/compose/11-stranded-delivering.sh
```

### Result — prediction correct

40 events, 8 in flight at the kill:

```
time       delivering   delivered    sink       dups
00:06:52Z  -- KILL -9, 8 requests in flight --
00:06:53Z  40           0            8          0
00:07:24Z  40           0            8          0
00:07:34Z  37           3            11         0     <- reclaim begins
00:07:55Z  30           10           18         0
00:08:05Z  4            36           44         7     <- dedup TTL expires
00:08:16Z  0            40           48         8
00:11:55Z  0            40           48         8

final: 8 duplicate (event, attempt) pairs, 0 still stranded
```

The duplicates appear **only after the marker expires**, 73 seconds after the
kill. And again: 8 in flight, 8 duplicates.

Raw output:
[`chaos/results/11-stranded-delivering.txt`](../chaos/results/11-stranded-delivering.txt)

### Why this matters

It corrects experiment 2. That experiment watches for ~3 minutes against a
15-minute dedup TTL and reports 0 duplicates — a correct number that supports
a wrong conclusion. The guard defers the duplicate past the end of the
observation window rather than eliminating it.

The underlying bug is that a worker which suppresses a dispatch acks the queue
entry **without recording an outcome**
(`internal/worker/worker.go:422-430`), so nothing ever resolves the event and
the marker eventually expires out from under it. Tracked as
[#19](https://github.com/singha105/webhook-relay/issues/19).

The general lesson is in the postmortem, and it is the one I would most want a
reviewer to notice: **an experiment that ends while the system is still in a
non-terminal state has not finished.**
