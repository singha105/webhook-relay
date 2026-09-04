# Postmortem: duplicate webhook deliveries

**Date:** 2026-09-03 (Day 5 chaos experiment), re-run with instrumentation 2026-09-04
**Status:** Resolved — control verified, one gap accepted and documented
**Severity:** SEV-2 equivalent. Customers double-processing webhooks.
**Author:** Arnab Singh
**Discovered by:** a chaos experiment I ran deliberately. Not by an alert.

---

## Summary

Killing a delivery worker while it is waiting for HTTP responses causes every
request that was in flight to be delivered a second time. The events are not
lost and nothing is corrupted on our side — the receiver simply gets the same
webhook twice, and if it is not idempotent, that is a double charge or a double
shipment.

The delivery-side deduplication guard added on Day 2 prevents this. This
experiment existed to find out whether that guard was actually load-bearing or
just a comforting block of code nobody had tested. It is load-bearing.

---

## Impact

Reproduced under controlled conditions, `docker kill --signal=KILL` on the
worker while deliveries were open:

| | Dedup disabled | Dedup enabled |
|---|---|---|
| Events posted | 400 | 400 |
| Deliveries the receiver got | **410** | 400 |
| Distinct events at the receiver | 400 | 400 |
| Duplicate `(event, attempt)` pairs | **10** | **0** |
| In-flight requests at kill time | 10 | 10 |

**2.5% of events were delivered twice** (10 of 400), inside a single kill.

The number that matters is not 10. It is that **10 was also the number of
requests in flight, which is also the worker's concurrency setting.** Not a
coincidence and not a probability — it is a certainty:

> Every request in flight at the moment a worker dies becomes a duplicate.

So the blast radius of one worker death is exactly `WORKER_CONCURRENCY`, and
the duplicate *rate* is `concurrency / events_in_the_window`. Raising
concurrency to 50 for throughput would have made a single kill produce 50
duplicates. The tuning knob and the correctness blast radius are the same
number, which is worth knowing before someone turns it up.

Window: 2 minutes 48 seconds from kill to full recovery, of which ~60s was
waiting out the stale-claim timeout by design.

Raw evidence, including the duplicated event IDs:
[`chaos/results/02-duplicate-delivery.txt`](../chaos/results/02-duplicate-delivery.txt)

---

## Timeline

All times UTC, from the instrumented re-run on 2026-09-04. Reproducible with
`./chaos/compose/02-duplicate-delivery.sh`.

| Time | Event |
|---|---|
| 23:17:01 | 400 events posted. Sink configured to hold each request open for 3s, so deliveries are reliably in flight rather than instantaneous. |
| 23:17:02 | Sink reports `in_flight: 10`. Ten HTTP requests have arrived at the receiver and have not been answered. **The kill is gated on this**, so the experiment tests what it claims to. |
| 23:17:02 | **Injection.** `docker kill --signal=KILL` on the worker. No SIGTERM, no drain, no chance to ack. |
| 23:17:05 | Worker restarts (3s). It does **not** pick up the orphaned entries — they are owned by the dead consumer's name and are invisible to a normal `XREADGROUP`. |
| ~23:18:05 | **Detection point.** Stale-claim timeout (60s) elapses. `XAUTOCLAIM` transfers the ten orphaned entries to the live worker. |
| ~23:18:05 | **The duplicates occur here**, not at the kill. The reclaiming worker has no way to know these ten were already sent, so it sends them again. |
| 23:19:50 | All 400 events terminal. Receiver has logged 410 deliveries. |

The gap between injection and duplication is the important part of this table.
The kill at 23:17:02 caused nothing directly — it created ten entries that
looked abandoned. The damage happened 60 seconds later, when the recovery
mechanism did exactly what it was designed to do.

---

## Detection

**Which metric surfaced it:** none. I found it because I went looking.

That deserves more than a one-word answer, because it is the least comfortable
part of this document.

The system has `webhook_deliveries_total`, `webhook_delivery_duration_seconds`,
queue depth, and a per-endpoint failure counter. Watch every one of them
through this incident and they all look **healthy**. Deliveries went up. Errors
did not. Latency was normal. Queue depth drained. From our side, ten extra
successful deliveries are indistinguishable from ten extra events — and by every
metric we export, the system did its job well.

**Would this have been caught in production? Honestly: no.** Not by us.

It would have been caught by a customer, some hours or days later, reporting
that they had been charged twice. Then someone would have spent an afternoon in
the delivery logs correlating `X-Webhook-Id` values before finding it.

The reason is structural and worth stating plainly: **a duplicate is only
observable at the receiver.** We know we sent two requests. We do not know the
receiver processed both, because "processed" is on the other side of the network
boundary — the same boundary that makes exactly-once impossible
([ADR 0004](adr/0004-at-least-once-over-exactly-once.md)). The receiver is the
only party who can see the duplicate, and the receiver is not the party who has
the dashboards.

What we can observe is the *precondition*, not the outcome, and that is the
basis of action item 1: `webhook_queue_reclaimed_total` counts entries recovered
via `XAUTOCLAIM`, and every one of those is a delivery that *may* have already
been sent. That is a real leading indicator, and it did not exist during this
incident.

---

## Root cause

The shallow answer is "the worker died." That is where it starts, not what
caused it. Workers are supposed to be able to die.

### The actual cause: we acknowledge after a side effect

The delivery loop does this:

```
1. claim the entry from the stream
2. send the HTTP request        <-- side effect, visible outside our system
3. write the result to Postgres
4. XACK the entry               <-- our record that step 2 happened
```

Steps 2 and 4 are not atomic and **cannot** be. They are in different systems,
and one of them is somebody else's server on the far side of the internet.

That gap is the bug. Between 2 and 4 there is a window where the side effect
has occurred and no durable record of it exists. A process that dies in that
window leaves the system in a state that is genuinely ambiguous — not
"unknown to us but knowable", but **actually indeterminate from any information
we hold.** The queue entry looks exactly like one that was never attempted.

Now consider the alternative, acking *before* sending. The window inverts: a
crash between ack and send means the entry is gone and the request was never
made. The event is silently lost, and no retry will ever fire because nothing
records that it needs one. **At-most-once.** We would have traded a duplicate
for a disappearance, and a disappearance is worse: a duplicate is loud and
fixable by an idempotent receiver, while a lost webhook is silent and
unrecoverable.

So the ordering is correct. Ack-after-side-effect is the deliberate choice, and
duplicates are its known cost.

### Why at-least-once becomes at-least-twice here

At-least-once says "we will retry until we are sure." Reclaim is how we become
sure. But `XAUTOCLAIM` cannot distinguish between:

- an entry claimed by a worker that died **before** sending, and
- an entry claimed by a worker that died **after** sending.

Both present identically: an entry, idle longer than the timeout, never acked.
The recovery mechanism has to assume the first — assuming the second would mean
silently dropping deliveries that never happened. It assumes correctly, and in
the second case it is wrong, and there is **no information available to it that
would let it tell the difference.**

That is the whole root cause. Not a missing check. A missing *fact*.

### What the guard actually does

The dedup guard adds a durable record of intent *before* the side effect:

```
SETNX delivery:{event_id}:{attempt}   <-- durable claim on this exact attempt
send the HTTP request
```

Now the reclaiming worker has the fact it was missing. It attempts the same
`SETNX`, fails, and knows this attempt was already dispatched by someone.

The subtlety is what this does **not** fix. The key is
`(event_id, attempt)` — it makes one *attempt* dispatch-once. It does nothing
about a timeout, which produces a legitimately different attempt number that
*should* be sent again. Chaos experiment 10 is that case, and it is worse: a
receiver answering at 10.4s against our 10s timeout **successfully processed 15
requests while we recorded zero deliveries.** The guard is silent there, and
correctly so.

**The window is narrowed, not closed. It cannot be closed from our side.** The
only complete fix lives in the receiver, and that is the contract.

---

## Contributing factors

1. **The stale-claim timeout is 60 seconds.** Recovery is fast, which is good,
   and it also means the duplicate lands quickly enough to race the original.
   A longer timeout would not prevent duplicates, only delay them, while
   lengthening real outages. There is no value that fixes this.

2. **The concurrency knob is also the blast-radius knob.** Nothing in the
   configuration says that raising `WORKER_CONCURRENCY` linearly raises the
   number of duplicates a single crash produces. Someone tuning for throughput
   would have no reason to suspect it.

3. **Dedup is behind a feature flag** (`DELIVERY_DEDUP_ENABLED`). Necessary to
   run this experiment, and a flag that disables a correctness control is a
   flag someone can turn off in production for a plausible-sounding reason.

4. **The guard fails open.** If Valkey is unavailable, `Claim` returns an error
   and delivery proceeds anyway. That is the right call — a possible duplicate
   beats a stalled pipeline — but it means the control silently stops working
   exactly when the system is already unhealthy.

5. **The sink's `in_flight` counter was not exposed in the deployed image**
   during the original Day 5 run. The kill was therefore not gated on requests
   actually being in flight; it produced duplicates anyway, but by luck rather
   than design. Fixed on Day 6, and the numbers in this document are from the
   corrected run.

---

## What went well

- **The control worked, and now it is proven.** 10 duplicates without it, 0 with
  it, same script, same load, one variable. That is a real experiment with a
  control, not an assertion.
- **Nothing was lost.** Every one of the 400 events reached a terminal state.
  The failure mode is duplication, never disappearance, which is the correct
  direction to fail in.
- **Recovery was automatic.** No human action. `XAUTOCLAIM` found the orphaned
  entries and the pipeline drained on its own.
- **The design anticipated this.** The guard existed before the experiment; the
  experiment tested it rather than discovering the need for it.
- **The experiment is committed and repeatable.** Anyone can run
  `./chaos/compose/02-duplicate-delivery.sh` and get this result. If the guard
  regresses, this fails.

---

## Action items

| # | Action | Owner | Status |
|---|---|---|---|
| 1 | Add `webhook_queue_reclaimed_total`, incremented on every `XAUTOCLAIM` recovery, and alert when it is non-zero. The only leading indicator of duplicates we can observe from our side. | @singha105 | [#9](https://github.com/singha105/webhook-relay/issues/9) |
| 2 | Document the concurrency ↔ blast-radius relationship where the knob is set, not only in this postmortem. | @singha105 | [#10](https://github.com/singha105/webhook-relay/issues/10) |
| 3 | Emit a startup warning when `DELIVERY_DEDUP_ENABLED=false`, and add a readiness-visible flag so a cluster running without the guard is obvious. | @singha105 | [#11](https://github.com/singha105/webhook-relay/issues/11) |
| 4 | Publish a receiver-side idempotency example — real code, not prose — showing `X-Webhook-Id` deduplication. The contract is only as good as the number of receivers that implement it. | @singha105 | [#12](https://github.com/singha105/webhook-relay/issues/12) |
| 5 | Count guard fail-open events separately so "the control was unavailable" is distinguishable from "the control passed". | @singha105 | [#13](https://github.com/singha105/webhook-relay/issues/13) |

Deliberately **not** doing: closing the window entirely. It cannot be closed
from the sender side, and building machinery that appears to would be worse than
documenting the limit.

---

## Lessons learned

**A control nobody has tested is a hypothesis.** The dedup guard was written on
Day 2 with a confident comment explaining why it was necessary. It was correct.
It could equally have been subtly wrong — keyed on the wrong tuple, with a TTL
shorter than the reclaim timeout — and everything would have looked fine, right
up until it mattered. The difference between Day 2 and Day 5 is not that the
code changed. It is that the claim became falsifiable.

**Healthy dashboards are not evidence of correct behaviour.** Every metric was
green throughout. The metrics measure what we did, and the incident is defined
by what the *receiver* experienced. When the thing you care about happens on the
other side of a network boundary, instrumentation on your side can only ever
show you a proxy — and it is worth being explicit about which of your metrics
are proxies and what they are standing in for.

**The interesting part of a distributed-systems bug is usually an absence.**
There is no missing line of code here. `XAUTOCLAIM` is correct, the ack ordering
is correct, the retry logic is correct. What is missing is a *fact* — nothing in
the system records that a request was sent. Once framed that way the fix is
obvious (write the fact down first) and so is its limit (you can only record
intent, never the receiver's outcome).

**Failure modes have a direction, and you should choose it deliberately.** Every
ordering here trades duplication against loss. We chose duplication, on purpose,
because a duplicate is loud and a loss is silent. That decision should be
explicit in a design document rather than an accident of the order somebody
wrote four lines in.

**A tuning knob can be a correctness knob.** `WORKER_CONCURRENCY` looks like a
pure throughput dial. It is also, exactly, the number of duplicates one crash
produces. Nobody would have found that by reading the code — it fell out of
instrumenting the experiment and noticing that three numbers matched.
