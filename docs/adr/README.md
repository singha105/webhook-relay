# Architecture decision records

Six decisions that shaped this system, each with the context that forced the
choice and the consequences of living with it.

The format is deliberately short. An ADR that runs to five pages does not get
read, and the point of writing one is that the next person — including me in
six months — can reconstruct *why* without excavating the git history.

| # | Decision | Status |
|---|---|---|
| [0001](0001-valkey-streams-as-the-queue.md) | Valkey Streams as the queue | Accepted |
| [0002](0002-transactional-outbox.md) | Transactional outbox for enqueueing | Accepted |
| [0003](0003-full-jitter-backoff.md) | Full-jitter exponential backoff | Accepted |
| [0004](0004-at-least-once-over-exactly-once.md) | At-least-once over exactly-once | Accepted |
| [0005](0005-gitops-pull-over-push.md) | GitOps pull-based delivery over push CD | Accepted |
| [0006](0006-sealed-secrets-over-cloud-kms.md) | Sealed Secrets over a cloud KMS | Accepted |
