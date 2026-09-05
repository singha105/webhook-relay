# 3. Full-jitter exponential backoff

**Status:** Accepted

## Context

A failed delivery is retried. The question is when.

Plain exponential backoff — `base · 2^attempt` — has a failure mode that only
appears at scale, and it appears exactly when the system can least afford it.
If an endpoint goes down while 500 events are in flight, all 500 fail at
roughly the same moment, and all 500 retry at *exactly* the same moment.
Backoff spaces the waves out in time but keeps every wave synchronised. The
endpoint comes back up and is immediately hit by a thundering herd, which knocks
it down again.

The retry policy is therefore not really about politeness. It is about not
turning one outage into a self-sustaining one.

## Decision

Full jitter:

```
delay = U(0, min(cap, base · 2^attempt))
```

A uniform random draw over the whole interval, not a small perturbation around
the exponential value.

Six attempts, 1s base, 1h cap, then the dead letter queue.

## Why full jitter and not the others

The three usual candidates, from
[AWS's analysis](https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/):

- **No jitter** — `base · 2^n`. Perfectly synchronised herds.
- **Equal jitter** — `half + U(0, half)`. Halves the clustering but keeps a
  guaranteed floor, so the herd is spread across half the window instead of all
  of it.
- **Full jitter** — `U(0, full)`. Maximum spread.

Full jitter spreads retries across the entire window, which is what actually
decorrelates the herd. Its cost is that an individual retry can fire almost
immediately, so a single event's *expected* delay is halved — worse for that
one event, better for the endpoint everything else is also trying to reach.

That trade is correct here because the circuit breaker
handles the "endpoint is genuinely down" case, so a fast retry is not the
mechanism protecting the receiver — the breaker is. Jitter only has to solve
correlation.

## Consequences

**Good.** Retries from a mass failure are spread uniformly. Verified indirectly
by chaos experiment 4's design: the retry timestamps for a batch that failed
together do not cluster.

**Bad.** Retry timing is not reproducible, which makes tests that assert on it
awkward. The tests assert on the *bounds* — never negative, never above the cap,
distributed across the range — rather than on values.

**Bad — genuinely unresolved.** With a 1h cap and 6 attempts, worst-case total
delivery time is several hours, and support cannot tell a customer when a retry
will land, only that it will. A visible `next_retry_at` in the API partly
compensates.

**Sharp edge.** `next_retry_at` doubles as the lease expiry while an event is
`delivering`. That is one column serving two purposes, and it works only
because both mean "do not touch this until". It saves a column and an index at
the cost of a comment explaining it, which is the right trade at this size and
would not be at ten times the complexity.
