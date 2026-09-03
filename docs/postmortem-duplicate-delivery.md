# Postmortem: duplicate webhook deliveries

**Status:** resolved, with a control that is now tested rather than assumed
**Severity:** would have been SEV-2 in production — customers double-processing
**Discovered by:** a chaos experiment, not an incident

---

## Summary

Killing a delivery worker between "HTTP request sent" and "queue entry
acknowledged" causes the same event to be delivered twice. The delivery-side
deduplication guard added on Day 2 prevents it. This experiment exists to prove
the guard is load-bearing rather than decorative, by removing it and watching
the duplicates appear.

For a webhook receiver that is not idempotent, a duplicate is a double charge,
a double shipment, or a double email.

---

## The window

Delivery is at-least-once. That is a deliberate design decision, and it means
there is a window that cannot be closed:

```
  worker claims entry from the stream
       |
       |  <-- kill here: nothing has happened. Entry is reclaimed. Fine.
       |
  worker sends HTTP POST  ------------------>  receiver processes it
       |
       |  <-- KILL HERE. The receiver HAS the event.
       |      The queue does not know that.
       |
  worker XACKs the entry
       |
       |  <-- kill here: already acked. Fine.
```

A kill in the middle band leaves an entry that was delivered but never
acknowledged. After `STALE_CLAIM_TIMEOUT` (60s) `XAUTOCLAIM` hands it to
another worker, which has no way to know the request already went out — so it
sends it again.

This is not a bug in the queue. It is the fundamental limit of at-least-once
delivery across a network boundary: **we cannot distinguish "the receiver never
got it" from "the receiver got it and the acknowledgement was lost."**

---

## The control

Before dispatching, a worker claims a key in Valkey:

```
SETNX delivery:{event_id}:{attempt}   TTL 15m
```

If the key already exists, another worker has already dispatched this exact
attempt, and this one must not.

Two details that matter:

**Keyed on (event, attempt), not on event.** Retries are supposed to happen —
attempt 2 after attempt 1 fails is correct behaviour. Only a redelivery of the
*same attempt number* is a duplicate.

**It fails open.** If Valkey is unreachable the delivery proceeds. Refusing to
deliver because a cache is down would convert a duplicate-delivery risk into a
total delivery outage, and the receiver is already required to tolerate
duplicates.

---

## What the control does NOT fix

It narrows the window. It does not close it.

The gap between claiming the key and the request reaching the receiver is still
unprotected — a worker that sets the key and then dies has blocked the
legitimate retry until the TTL expires. Reversing the order trades that for the
duplicate we are trying to prevent. There is no ordering that yields
exactly-once.

It also does nothing for the case in
[experiment 10](../chaos/10-receiver-at-timeout-boundary.yaml): a receiver that
responds successfully at 9.9 seconds against a 10-second timeout. We time out,
classify it retryable, and send attempt 2 — a *different* attempt number, so
the guard correctly does not suppress it. The receiver processes the event
twice and both are legitimate deliveries by our contract.

**The only complete fix is on the receiver.** Every delivery carries a stable
`X-Webhook-Id`, and [`pkg/webhook`](../pkg/webhook) tells integrators to
deduplicate on it — with the reason stated, because "deduplicate on this" with
no explanation is advice people skip.

---

## Reproducing it

```bash
./chaos/compose/02-duplicate-delivery.sh
```

The script runs the identical scenario twice — post events, wait until requests
are genuinely in flight at the sink, `kill -9` the worker, restart it, wait out
the stale-claim reclaim — once with `DELIVERY_DEDUP_ENABLED=false` and once
with `true`, and compares what the sink recorded.

The sink counts a duplicate only when the same `(event_id, attempt_number)`
pair arrives more than once. A repeated event id across *different* attempts is
an ordinary retry and is not counted.

The Kubernetes form is
[`chaos/02-kill-worker-dedup-disabled.yaml`](../chaos/02-kill-worker-dedup-disabled.yaml).

---

## Results

See [chaos-results.md](chaos-results.md#experiment-2).

---

## What this changes

Nothing in the code — the guard was already there. What changed is that it is
now **tested**: there is a committed, repeatable experiment that fails if the
guard regresses.

A safety control that has never been observed failing is indistinguishable from
one that does nothing. That is the entire reason `DELIVERY_DEDUP_ENABLED`
exists as a flag rather than being hardcoded to `true`.
