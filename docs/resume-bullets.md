# Resume bullets

Four bullets. Every number traces to a committed script you can re-run.

The confidence notes are the point of this document: if you are asked to defend
one of these in an interview, the honest answer and its caveat are here, so you
are never defending a number you cannot reconstruct.

---

### 1. Delivery guarantee, proven by fault injection

> Built a webhook delivery service with an at-least-once guarantee, then proved
> the deduplication control was load-bearing by SIGKILLing workers mid-delivery:
> **10 duplicate deliveries across 400 events without the guard, 0 within the
> observation window with it.** Discovered the duplicate count equals the
> worker's concurrency setting exactly, making the throughput knob also the
> correctness blast radius.

**Verification:** `./chaos/compose/02-duplicate-delivery.sh` — same script, two
runs, one variable. Raw output in `chaos/results/02-duplicate-delivery.txt`.

**Confidence: high, with one caveat you should state before being asked.** The
"0 with it" is measured over a ~3 minute window against a 15-minute dedup TTL.
A follow-up experiment showed the guard *defers* the duplicate rather than
eliminating it. **Say this yourself** — "and when I extended the observation
window, I found the control was weaker than I'd claimed" is a much stronger
answer than being caught by it. The concurrency-equals-blast-radius finding is
solid: `in_flight = 10, duplicates = 10, WORKER_CONCURRENCY = 10`, and the
mechanism explains why it must be so.

---

### 2. Throughput, and finding the bottleneck by elimination

> Load-tested with k6 to **~875 events/sec ingest and ~870/sec delivery** on a
> single 8-core laptop. Located the bottleneck by elimination rather than
> guesswork — disproving pool size and worker concurrency, then using
> `docker stats` to show nothing was CPU-saturated — and traced it to the outbox
> relay making O(n) round trips per batch. Pipelining the enqueue improved
> sustained throughput **13.1% (769 → 870 events/sec)**.

**Verification:** `k6 run loadtest/baseline.js`, `./loadtest/drain-throughput.sh`,
3 repeats. Before/after in `loadtest/results/optimization-relay-pipelining.md`.

**Confidence: high on the numbers, and be precise about attribution.** The
pipelining change alone is **+5.4%** (769 → 811); the full 13.1% needs
concurrency and pool raised alongside it. Do not let "13.1%" imply one change
did it. Also volunteer the unresolved part: 5× the concurrency buys only 7% on
an unsaturated machine, so a serialization point remains unidentified
(issue #6). These are laptop numbers on a single node and should always be
introduced as such.

---

### 3. Kubernetes deployment with pull-based GitOps

> Deployed on Kubernetes (k3d) provisioned entirely by Terraform, with Helm
> charts, ArgoCD app-of-apps and sync waves, Cosign-signed images with SBOM
> attestation, default-deny NetworkPolicies, PodDisruptionBudgets, and
> multi-window burn-rate alerting against a **99.5% delivery SLO** — chosen so CI
> never holds a cluster credential.

**Verification:** `terraform/`, `gitops/`, `deploy/helm/`, and the CI workflows.
`make bootstrap` provisions it.

**Confidence: medium — know exactly what to concede.** This was built and the
manifests are real, but it is **not currently verified running end to end**: the
full profile needs ~6 GiB of Docker memory and the machine has 3.825 GiB, so the
cluster path is not part of `make demo`. If asked "did you see it work?", the
honest answer is that components were verified during Day 4 and the full stack
was not brought up together afterwards. Lead with the *reasoning* — pull-based
CD so a compromised pipeline is not a compromised cluster
(`docs/adr/0005`) — which is the part worth interviewing about anyway. Do not
claim production operation.

---

### 4. Chaos engineering with falsifiable predictions

> Designed 11 chaos experiments with predictions and falsification criteria
> committed **before** each run, and recorded the results including the two that
> refuted my own predictions — one of which revealed that a receiver 400ms slower
> than the delivery timeout **successfully processed 15 requests while the system
> recorded zero deliveries.** Wrote blameless postmortems and opened 11 tracked
> issues from the findings.

**Verification:** predictions in `chaos/*.yaml` (in git history, before the
result commits), results in `docs/chaos-results.md`, postmortems in `docs/`.

**Confidence: high.** This is the most defensible bullet because the predictions
are timestamped in git before the results. The 15-requests/0-deliveries figure
is directly measured. Be ready to explain *why* it happens — the two generals'
problem, and that a timeout is an unknown rather than a failure — because that
is what the bullet is really claiming to understand. Note that 5 of the 11 did
not run, for the memory reason above; the log says so explicitly, which is
itself worth pointing at.

---

## Numbers I would not put on a resume

Flagged because the temptation exists:

- **"Handles 5000 requests/sec."** The ramp scenario *targets* 5000/s. It was
  not sustained; that is the load profile, not a result.
- **"99.5% availability."** That is the SLO the alerting is written against, not
  a measured uptime. There is no production deployment to measure.
- **"Zero data loss."** True in every experiment run so far, but 5 of 11
  experiments have not run. "No events lost in any experiment I ran" is
  defensible; the unqualified version is not.
- **"Reduced latency by X%."** Not measured. The work targeted throughput, and
  p95 ingest latency was a threshold the tests had to stay under, not something
  optimized.
- **"Production-grade" / "production-ready."** It has never served production
  traffic. "Built to production practices" is the honest phrasing.
