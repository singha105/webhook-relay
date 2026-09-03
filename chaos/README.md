# Chaos experiments

Ten experiments, each committed as a manifest so it is reproducible rather than
a story about something that happened once.

Every experiment follows the same discipline:

1. **Predict** what will happen, in writing, before running it.
2. **Run** it under load.
3. **Record** what actually happened.
4. **Say so when the prediction was wrong** — that is the interesting case, and
   an experiment log with no surprises in it is one nobody learned from.

Results: [`docs/chaos-results.md`](../docs/chaos-results.md).

## Running one

```bash
kubectl apply -f chaos/01-kill-worker-mid-delivery.yaml
kubectl -n chaos-testing get podchaos -w
kubectl delete -f chaos/01-kill-worker-mid-delivery.yaml
```

They target the `webhook-relay` namespace and select on the labels the Helm
chart applies, so they follow pods across a rollout rather than pinning a name
that changes.

## What needs what

| Experiment | Requires |
|---|---|
| 01, 02, 03, 05, 07, 10 | Chaos Mesh — or the Compose equivalent in [`compose/`](compose/) |
| 04, 08 | Chaos Mesh NetworkChaos; no Compose equivalent |
| 06 | CloudNativePG with 3 or more replicas |
| 09 | Chaos Mesh StressChaos |

The Compose-reproducible ones have shell equivalents, because an experiment
that only runs on infrastructure the reader cannot start is one they have to
take on trust.
