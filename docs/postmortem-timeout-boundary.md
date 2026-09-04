# Postmortem: deliveries the receiver completed and we recorded as failures

**Date:** 2026-09-03 (Day 5, chaos experiment 10)
**Status:** Resolved as "working as designed", which is the uncomfortable answer
**Severity:** SEV-2 equivalent for any receiver that is slow and not idempotent
**Author:** Arnab Singh

---

## Summary

A receiver that answers correctly, but slightly slower than our 10-second
timeout, gets every webhook delivered repeatedly and we record all of it as
failure. In the reproduction: **the receiver successfully processed 15 requests
and our system recorded zero successful deliveries.**

This is not a bug in the sense that some line of code is wrong. Every component
did exactly what it should. It is the at-least-once contract meeting a slow
receiver, and it is the case the duplicate-delivery guard explicitly does not
cover.

---

## Impact

Sink configured to return HTTP 200 after 10.4s, against a client timeout of 10s:

| | |
|---|---|
| Events posted | 10 |
| Attempts we made | 15 |
| Attempts we recorded as timeouts | **15 (100%)** |
| Deliveries we recorded as successful | **0** |
| Requests the receiver actually completed | **15** |
| Distinct events the receiver saw | 8 |
| Events processed more than once | **5** |

Read the last three rows together. The receiver did fifteen units of real work.
Our database says nothing was delivered. Had the run continued through all six
attempts, each event would have been processed up to six times and then
dead-lettered — reported to the customer as *undeliverable* after being
delivered six times.

Control: the same experiment at 9.9s — 100ms inside the timeout — produced 30
events, 30 attempts, zero timeouts, zero duplicates. The behaviour is a cliff,
not a slope. Nothing degrades gracefully; you are on one side of 10s or the
other.

Raw evidence:
[`10-timeout-boundary-over.txt`](../chaos/results/10-timeout-boundary-over.txt),
[`10-timeout-boundary-under.txt`](../chaos/results/10-timeout-boundary-under.txt)

---

## Timeline

This is a synthetic experiment, so the timeline is the mechanism rather than a
sequence of discoveries:

| T | What happens |
|---|---|
| 0.0s | Worker sends the delivery. |
| 10.0s | Our client gives up. We record a timeout: `status_code = NULL`, classified retryable. The connection is closed. |
| 10.4s | The receiver finishes its work and writes a 200 to a socket nobody is listening on. **From its perspective the delivery succeeded.** |
| ~11s+ | Backoff elapses. We send attempt 2. The receiver processes the same event a second time. |
| … | Repeats to `MAX_ATTEMPTS`, then dead-letters. |

---

## Detection

Not detected by monitoring, and unlike the duplicate-delivery incident, this one
*would* be caught in production — by the wrong signal, pointing the wrong way.

`webhook_deliveries_total{result="timeout"}` would spike, and the circuit
breaker would open on consecutive failures. On-call would see "endpoint X is
failing" and go looking for a broken receiver. The receiver's own dashboards
would show 100% success at p99 ≈ 10.4s.

Two teams, both with correct data, reaching opposite conclusions. The
escalation would be resolved by someone noticing that our timeout and their p99
are 400ms apart.

Worth naming: our metrics are not wrong here. They accurately report what we
observed. The observation is just not the same thing as what happened.

---

## Root cause

**A timeout is not a failure. It is an unknown, and we are forced to record it
as a failure because that is the only safe assumption available.**

When a request times out, two worlds are consistent with everything we can see:

1. The receiver never got it, or got it and failed.
2. The receiver processed it successfully and the response did not arrive in
   time.

We cannot distinguish these — the two generals' problem, and no amount of
engineering removes it ([ADR 0004](adr/0004-at-least-once-over-exactly-once.md)).
Given the ambiguity we must pick, and we pick "retry", because the alternative
is silently dropping events that genuinely failed.

That assumption is correct for a broken receiver and wrong for a slow one. The
cruelty is in *which* case it is wrong for: **the system inflicts its worst
behaviour — repeated duplicate processing — on the receivers least able to
absorb it.** A receiver near the timeout boundary is one that is struggling, and
our response to a struggling receiver is to send it the same work five more
times.

### Why the dedup guard does not help

The guard keys on `(event_id, attempt)`. A timeout produces attempt N+1 — a
genuinely new attempt that *should* be dispatched, because as far as we can tell
attempt N never landed. The guard does not fire, and it is right not to.

The guard prevents us from sending the *same attempt* twice. It has nothing to
say about sending a *new attempt* for work that already succeeded. Those are
different problems and only the first is solvable on our side.

---

## Contributing factors

1. **The timeout is global**, not per-endpoint. One 10s value for every
   receiver, regardless of how slow that receiver is known to be.
2. **A timeout is classified identically to a connection refusal.** Both are
   "retryable, status code NULL". A refusal is unambiguous — nothing was
   processed. A timeout is ambiguous. Merging them discards the distinction at
   the exact point it matters.
3. **Nothing detects proximity to the boundary.** A receiver sitting at 9.8s is
   one deploy away from this incident and no signal says so.
4. **The dead-letter reason will say "timeout"**, which reads as "the receiver
   never got it" — actively misleading for the case where it got it six times.

---

## What went well

- **The experiment found this before a customer did**, which is the entire
  argument for chaos testing. Nothing in code review would have surfaced it.
- **The prediction was wrong in an instructive way** (see below), and the wrong
  version is recorded in [chaos-results.md](chaos-results.md) alongside the
  right one.
- **Retries were correctly bounded.** Six attempts and then dead-lettered, not
  an infinite loop against a receiver that is already slow.
- **Timeouts are recorded distinguishably** — `status_code IS NULL` — so the
  reproduction could count them exactly.

---

## Action items

| # | Action | Owner | Status |
|---|---|---|---|
| 1 | Per-endpoint configurable delivery timeout. One global value cannot fit both a 200ms API and a receiver doing synchronous work. | @singha105 | [#14](https://github.com/singha105/webhook-relay/issues/14) |
| 2 | Alert on `delivery_duration p99 > 0.8 × timeout` per endpoint — the leading indicator for a receiver about to fall off the cliff. | @singha105 | [#15](https://github.com/singha105/webhook-relay/issues/15) |
| 3 | Distinguish timeout from connection-refused in attempt records and metrics. A refusal means nothing was processed; a timeout means we do not know. | @singha105 | [#16](https://github.com/singha105/webhook-relay/issues/16) |
| 4 | State on dead-lettered timeouts that the event may have been delivered. The current wording implies it certainly was not. | @singha105 | [#17](https://github.com/singha105/webhook-relay/issues/17) |
| 5 | Make receiver-side idempotency prominent in the docs, with the worked example from [#12](https://github.com/singha105/webhook-relay/issues/12). | @singha105 | [#12](https://github.com/singha105/webhook-relay/issues/12) |

---

## Lessons learned

**My prediction was wrong, and the way it was wrong is the lesson.** I predicted
double-processing and set the sink to 9.9s against a 10s timeout — "close
enough to cross sometimes." It never crossed. Thirty events, thirty attempts,
zero timeouts. I had reasoned about the mechanism correctly and then chosen a
parameter that cannot trigger it, because 100ms of headroom is far larger than
any jitter on a local Docker bridge. **"Close to the boundary" is intuition;
whether the gap exceeds the variance is an empirical question**, and I had not
asked it. The experiment only worked once I stopped approximating the boundary
and actually crossed it.

**Ambiguity should be preserved in the data model, not collapsed at the edge.**
We know at the HTTP client that a timeout and a refusal mean different things.
We throw that away by storing both as `status_code = NULL`, and every consumer
downstream — metrics, the breaker, the dead-letter reason — inherits a
distinction that no longer exists. Cheap to keep, expensive to reconstruct.

**Check whether your worst behaviour is aimed at your weakest dependency.** This
system responds to a slow receiver by multiplying its load sixfold. That is
backwards, and it took an experiment to see it, because in isolation every
component is behaving correctly. It is worth asking of any retry policy: who
receives the retries, and are they in a position to handle them?

**The clean run was more informative than the dirty one.** The 9.9s result
looked like a wasted experiment — no effect, nothing learned. It was the control
that made the 10.4s result mean something, and it is what proves the behaviour
is a cliff rather than a gradient. Negative results are only worthless if you do
not write them down.
