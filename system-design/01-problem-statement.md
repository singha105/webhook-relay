# Problem statement

**An application needs to tell someone else that something happened, over a
network that loses messages, to a server it does not control and cannot fix.**

Every SaaS product eventually has to notify its customers: a payment settled, a
build finished, a document was signed. The naive implementation is four lines —
`POST` the JSON and move on. It works in development and fails in production,
because the receiving server is outside your control and will be broken,
overloaded, slow, or briefly unreachable, and none of that is your customer's
fault or yours.

Doing it properly is not hard *code*, it is a pile of unpleasant edge cases that
have to be handled somewhere:

| The situation | What has to happen |
|---|---|
| Receiver returns 503 for ten minutes | Retry with backoff, without hammering them into staying down |
| Receiver is permanently gone | Stop eventually, and make the failure visible rather than silent |
| Your own process dies mid-send | Recover the work — without spamming a receiver that already got it |
| Receiver is slower than your timeout | Decide what a timeout even *means*, because it is not a failure |
| 500 events fail at once, then the receiver recovers | Do not deliver all 500 in the same millisecond and kill it again |
| Receiver asks "did you really send this?" | Be able to prove it, months later |
| A customer's endpoint gets DDoSed by your retries | Rate limit per endpoint, and stop calling a dead one entirely |

Written inline, that logic gets duplicated in every service that sends
notifications, implemented slightly differently each time, and tested by nobody.
**This project extracts it into one component with one job.** The application
hands over an event and forgets about it; the relay owns delivery, retries,
backpressure, and the audit trail.

### Requirements it was built against

| | Requirement |
|---|---|
| **Functional** | Accept an event and durably store it before acknowledging |
| | Deliver to a registered HTTPS endpoint, signed, so the receiver can verify origin |
| | Retry transient failures; give up on permanent ones; never fail silently |
| | Make every attempt inspectable after the fact |
| | Allow a failed event to be replayed once the receiver is fixed |
| **Non-functional** | No event may be lost once accepted — duplicates are acceptable, loss is not |
| | One slow receiver must not stall delivery for everyone else |
| | Survive the loss of any single component without losing accepted events |
| | Be observable enough to debug a specific customer's specific event |
| **Constraint** | Entirely free and open source, self-hosted, no cloud account anywhere |

### What it is not

Not a message broker (it does not do fan-out or pub/sub — use Kafka or NATS),
not a task queue for internal jobs (use a job runner), and not a notification
service for humans (no email, SMS, or push). It does exactly one thing: deliver
HTTP callbacks to third-party endpoints, reliably and accountably.

---

> ### 📄 Start here: [Postmortem — duplicate webhook deliveries](../docs/postmortem-duplicate-delivery.md)
>
> I killed a worker mid-delivery and measured what broke. Every request in
> flight was delivered **twice** — and the number of duplicates turned out to
> be exactly the worker's concurrency setting, which means the throughput knob
> is also the correctness blast radius. The write-up covers why acking after a
> side effect makes this unavoidable, and why no metric we export would have
> caught it in production.
>
> Second one: [deliveries the receiver completed and we recorded as
> failures](../docs/postmortem-timeout-boundary.md) — a receiver 400ms slower than
> our timeout processed 15 requests while we recorded zero.

---

---

**Next:** [High-level design →](02-high-level-design.md)
