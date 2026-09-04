# 6. Sealed Secrets over a cloud KMS

**Status:** Accepted

## Context

[ADR 0005](0005-gitops-pull-over-push.md) makes Git the source of truth for
everything deployed. Secrets are deployed. Git is public.

A Kubernetes `Secret` is base64, which is an encoding, not encryption — a
committed Secret manifest is a committed secret. So something has to encrypt
values before they reach Git and decrypt them inside the cluster.

There was also a hard project constraint: **free, open source, self-hosted, no
account, no credit card.** That is not incidental here — it eliminates the
industry-default answer outright.

## Decision

**Bitnami Sealed Secrets.** A controller in the cluster holds a private key and
generates a public certificate. `kubeseal` encrypts values against that
certificate on a developer machine, producing a `SealedSecret` that is safe to
commit. The controller decrypts it into a real `Secret` at apply time.

Encryption is scoped to namespace and name by default, so a SealedSecret cannot
be moved to a namespace where it would be readable by something else.

## Why not the alternatives

**AWS/GCP/Azure KMS with External Secrets Operator** is the industry default and
is what I would use in a company. It is disqualified here by the project
constraint — it requires a cloud account and a credit card — and that
constraint is the point of the exercise rather than an obstacle to it. Worth
being explicit: for a real production system with an existing cloud footprint,
KMS plus External Secrets is the better answer, mostly because of key rotation
and audit logging.

**HashiCorp Vault** is self-hostable and free, and is the strongest technical
alternative. Rejected on operational weight: Vault needs unsealing, its own
storage backend, its own HA story and its own upgrade path. That is a larger
operational commitment than the entire application it would be protecting.

**SOPS with age** was the closest call and would also have been defensible. It
is simpler than Sealed Secrets — a file format and a binary, no controller — and
decryption can happen at apply time. Sealed Secrets won on cluster-native
ergonomics: the controller reconciles SealedSecrets into Secrets continuously,
so a restored cluster re-materialises its secrets with no human running a
decrypt step. SOPS would have needed that step wired into the delivery path.

## Consequences

**Good.** Encrypted secrets live in Git alongside everything else. One source of
truth, one review process, no out-of-band "and also copy these values by hand"
step that inevitably rots.

**Good.** The private key never leaves the cluster. A developer can seal a
secret but cannot unseal one — asymmetric encryption, so write access does not
imply read access.

**Bad — the failure mode to design around.** The controller's private key is
the entire security boundary *and* the entire recovery story. Lose it and every
committed SealedSecret is permanently undecryptable; leak it and every committed
SealedSecret is readable, including from Git history. `make seal-backup` exports
it, the output is gitignored, and [secrets.md](../secrets.md) documents the
restore path. This is the sharpest edge in the deployment.

**Bad.** No rotation story worth the name. Rotating the controller key means
re-sealing every secret. A cloud KMS would give rotation, per-key IAM, and an
audit log of every decrypt; Sealed Secrets gives none of those.

**Bad.** No audit trail. Nothing records that a secret was read.

**Bad.** Another controller to run and upgrade, and its CRD version is coupled
to committed manifests.
