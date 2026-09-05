# Contributing

## Local setup

Docker, Go 1.25+, `curl`, `jq`. Nothing else — no cloud account, no API key.

```bash
git clone https://github.com/singha105/webhook-relay.git
cd webhook-relay
make demo        # builds, starts, migrates, and proves it works
```

`make help` lists every target.

## Before you open a pull request

```bash
make fmt
make lint        # golangci-lint, must be clean
make test        # needs Docker: integration tests use testcontainers
```

CI runs the same commands plus a Trivy image scan, a gitleaks history scan, and
a check that the compose stack comes up clean. All six must pass.

## Testing conventions

**Integration tests use real dependencies.** Anything touching Postgres or
Valkey runs against the real thing in a container via `testcontainers-go`.
There are no database mocks here, and adding one would be a regression: a mock
would have cheerfully passed the idempotency race that a real Postgres caught
on Day 1, and the `pgxpool` lazy-connection bug that made a race test pass
against a broken implementation.

Table-driven tests for pure logic — signing, backoff, classification, config
parsing.

**Test names should state the property, not the method.** Prefer
`TestRelayReleasesClaimsItCouldNotEnqueue` over `TestRelayOnce`. The name is
what someone reads when it fails at 3am.

**Assert on stable quantities.** A test that samples an oscillating value after
a fixed sleep is a flaky test, and a flaky test is worse than no test — it
trains people to re-run CI. If you find yourself writing `time.Sleep` before an
assertion, ask what invariant you could assert instead that does not depend on
timing.

## Commits

Conventional commits with a scope:

```
feat(worker): ...    fix(store): ...     test(relay): ...
docs(readme): ...    perf(relay): ...    chore(ci): ...
```

The commit body is where the reasoning goes. A message that says *what*
changed duplicates the diff; one that says *why*, and what was tried and
rejected, is the only place that information will ever exist. If a measurement
motivated the change, put the numbers in the message.

## Branches

Short-lived branches off `main`, pull request, squash-free merge. `main` is
always green and always deployable.

## Changing the data model

Migrations are append-only and live in `/migrations`. Never edit a migration
that has been merged — add a new one.

Run `make chart-sync` afterwards so the Helm chart's copy cannot drift from the
canonical set.

## Adding a dependency

Ask first, in the issue or PR description. The dependency list is deliberately
short: chi, pgx, go-redis, otel, and the test libraries. Each addition is
something that has to be kept current, audited, and understood by whoever is
on call.

## Chaos experiments

New experiments go in `/chaos` as a manifest with two blocks written **before**
the run:

```yaml
# PREDICTION
#   What you expect to happen, specifically enough to be wrong.
# WHAT WOULD FALSIFY IT
#   What you would have to observe to conclude the prediction was wrong.
```

Then run it and record the result in `docs/chaos-results.md`, **including when
the prediction was wrong.** A prediction that was wrong is the most valuable
row in that table; experiment 10 is there precisely because I got it wrong.

Do not edit a prediction after seeing the result.

## Reporting a bug

Include the reproduction. A bug report against this repository should ideally
be a script someone can run — that is the standard the chaos experiments and
load tests are held to, and it is why `make demo` found two real bugs
([#18](https://github.com/singha105/webhook-relay/issues/18),
[#19](https://github.com/singha105/webhook-relay/issues/19)) that the test
suite did not.
