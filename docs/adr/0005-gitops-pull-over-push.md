# 5. GitOps pull-based delivery over push CD

**Status:** Accepted

## Context

CI builds an image. Something has to get that image running in the cluster.

The default is push: the CI job holds cluster credentials and runs
`kubectl apply` or `helm upgrade` at the end of the pipeline. It is simple,
it is what most tutorials show, and it has a property worth naming — **the CI
system becomes a standing credential to production**.

Any workflow file, any action, any dependency of any action, and anyone who can
open a pull request that triggers a workflow, is transitively inside that trust
boundary.

## Decision

Pull-based delivery with ArgoCD. **CI never holds a kubeconfig.**

CI builds the image, signs it, pushes it to GHCR, and writes the new tag into a
manifest in Git. ArgoCD runs *inside* the cluster, watches the repository, and
reconciles. The credential points inward, not outward.

Structured as app-of-apps with sync waves so infrastructure lands before the
things that depend on it. `selfHeal: true` and `prune: true`.

## Consequences

**Good — the one that matters.** Compromising CI no longer means compromising
the cluster. An attacker who owns the pipeline can write a bad tag into Git,
which is a reviewable, revertible, audited event, rather than a silent
`kubectl apply` of anything they like.

**Good.** Git is the desired state, so "what is deployed" is answered by reading
a file, and rollback is `git revert` rather than remembering which Helm release
number was good.

**Good.** `selfHeal` reverts manual drift automatically. Someone `kubectl edit`s
a Deployment at 3am to stop the bleeding, and ArgoCD puts it back — which is
correct behaviour and *infuriating* at 3am, so the runbook documents how to
suspend it deliberately.

**Bad.** Deploys are asynchronous. The pipeline goes green when the tag is
committed, not when the code is running. "Did it deploy?" needs a second
question, which is a real ergonomic loss and the most common complaint about
GitOps.

**Bad.** ArgoCD is a component to run, upgrade and monitor — meaningful weight
for a system this size. Justified here because the alternative is a standing
production credential in CI, and that trade only gets better as the system
grows.

**Bad.** Secrets cannot live in Git in plaintext, which forces the question that
[ADR 0006](0006-sealed-secrets-over-cloud-kms.md) answers.

**Sharp edge.** `prune: true` means deleting a manifest deletes the resource.
That is the point, and it is also how you remove a PersistentVolumeClaim by
tidying up a YAML file.
