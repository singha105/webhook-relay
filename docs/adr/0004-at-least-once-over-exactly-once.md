# 4. At-least-once over exactly-once

**Status:** Accepted

## Context

Every webhook system has to answer one question honestly: if we send a request
and never hear back, what do we do?

The answer determines the delivery guarantee, and there are only three
possibilities:

- **At-most-once** — do not retry. Never duplicates, silently loses events
  whenever a response is lost. Unacceptable for a delivery service.
- **Exactly-once** — retry, but guarantee the receiver processes it once.
- **At-least-once** — retry, and accept that the receiver may see it twice.

Exactly-once is what everybody wants and what most people believe they can
build.

## Decision

**At-least-once.** Stated in the API docs, the README, and the response
semantics, rather than buried.

## Why exactly-once is not available

This is the two generals' problem, and it is not an engineering difficulty —
it is a proof.

Two generals must agree to attack, communicating only by messenger through
territory where messengers are lost. General A sends "attack at dawn." Did it
arrive? A needs an acknowledgement. B sends one. Did *that* arrive? B now needs
an acknowledgement of the acknowledgement. The regress does not terminate:
**no finite protocol over a lossy channel can give both parties common
knowledge that a message was received.**

Our channel is HTTP over the internet, which loses messages. When a delivery
times out, there are two indistinguishable worlds:

1. The request never arrived. The receiver did nothing.
2. The request arrived, the receiver processed it, and the **response** was lost.

Nothing observable from our side separates these. We must choose:

- Assume (1) and retry → the receiver may process twice. **At-least-once.**
- Assume (2) and give up → we may silently drop an event nobody handled.
  **At-most-once.**

There is no third option. Systems advertising "exactly-once" are doing
at-least-once delivery plus deduplication *at the receiver* — which is exactly
what this ADR asks receivers to do, stated plainly instead of sold as a
guarantee we cannot make.

Chaos experiment 10 is the proof, not the metaphor. A receiver answering 200 at
10.4s against a 10s timeout: **the receiver successfully processed 15 requests
and our system recorded zero deliveries.** Both sides behaved correctly. The
channel lost the answer.

## What we do provide

1. **Every event is delivered at least once**, or dead-lettered after 6
   attempts and visible in the API. Never silently dropped.
2. **A stable `X-Webhook-Id`** — the same event carries the same id across every
   retry. This is the deduplication key, and it is the receiver's half of the
   contract.
3. **A narrowed duplicate window.** The delivery-side guard suppresses duplicate
   dispatch of the same `(event, attempt)` when a worker dies mid-delivery.
   Measured: 8 duplicates without it, 0 with it, over 300 events
   ([postmortem](../postmortem-duplicate-delivery.md)).
4. **Signed payloads**, so a retry is verifiable as ours and not a replay by
   someone else.

## What we explicitly do not provide

The guard narrows the window; it does not close it, and it **cannot**. It keys
on `(event_id, attempt)`, so it suppresses a duplicate *dispatch* of one
attempt. A timeout produces a legitimately new attempt number, and the guard
correctly does not fire — which is precisely the case where the receiver already
did the work.

**Receivers must be idempotent on `X-Webhook-Id`.** That is not a footnote, it
is the other half of the contract.

## Consequences

**Good.** The guarantee is honest, provable, and testable — and it *is* tested,
by an experiment that fails if the control regresses.

**Good.** No distributed-transaction machinery, no two-phase commit, no
coordination protocol that would be slower, more fragile, and still not
exactly-once.

**Bad.** We push work onto receivers, and some will not do it. They will get
duplicates and will be surprised, despite the documentation. This is the cost
of the honest answer, and every webhook provider pays it.

**Bad.** "At-least-once" reads as weaker than a competitor's "exactly-once"
even when the competitor is doing the same thing with better marketing.
