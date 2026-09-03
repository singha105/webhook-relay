# webhook-relay runbook

For someone on call who has never seen this system.

**What it does:** accepts webhook events over HTTP, stores them in Postgres,
and delivers them to customer-registered URLs with retries. At-least-once — a
receiver may see the same event twice and is expected to deduplicate on
`X-Webhook-Id`.

**The one number that matters:** `webhook_queue_oldest_message_age_seconds`.
It is how long the oldest undelivered event has been waiting since a customer
handed it to us. The SLO is 99% delivered within 30 seconds. If this is climbing,
customers are feeling it; if it is flat, most other symptoms are cosmetic.

---

## Orientation

```bash
kubectl config use-context k3d-webhook-relay

# What is running
kubectl -n webhook-relay get pods,deploy,hpa

# Is delivery actually happening
kubectl -n webhook-relay logs -l app.kubernetes.io/component=worker --tail=50

# The dashboard
open http://grafana.localhost:8081        # admin / `make endpoints`
```

| Thing | Where |
|---|---|
| API | `http://webhook-relay.localhost:8081` |
| Grafana | `http://grafana.localhost:8081` |
| ArgoCD | `http://argocd.localhost:8081` |
| Alerts received | `kubectl -n observability logs deploy/alert-receiver` |

The `.localhost` hostnames need an `/etc/hosts` entry — `make endpoints` prints it.

---

## Deploying

**You do not deploy by running a command.** Merging to `main` builds an image,
pushes it to GHCR, and commits the new tag to `gitops/values.yaml`. ArgoCD sees
that commit and rolls it out within ~30 seconds.

```bash
# Watch it happen
make argocd-status
kubectl -n webhook-relay rollout status deploy/webhook-relay-api
```

If ArgoCD has not picked it up, force a refresh:

```bash
make argocd-sync
```

**Migrations run automatically** as a Helm `pre-upgrade` hook, before any new
pod starts. If the migration fails, the deploy stops and the old version keeps
running — which is the correct outcome. Look at it with:

```bash
kubectl -n webhook-relay get jobs
kubectl -n webhook-relay logs job/webhook-relay-migrate-<revision>
```

---

## Rolling back

### Fast path: ArgoCD

```bash
# What revisions exist
argocd app history webhook-relay

# Roll back to a specific one
argocd app rollback webhook-relay <ID>
```

Or from `kubectl` if you do not have the ArgoCD CLI:

```bash
kubectl -n webhook-relay rollout undo deploy/webhook-relay-api
kubectl -n webhook-relay rollout undo deploy/webhook-relay-worker
```

> **This second form is temporary.** ArgoCD has `selfHeal` enabled, so it will
> revert your rollback within ~30 seconds and put the bad version back. Use it
> only to buy a minute while you do the real fix below.

### The real fix: revert the commit

Because deployment *is* a commit, rollback is a revert:

```bash
git revert <the deploy commit in gitops/values.yaml>
git push
```

ArgoCD reconciles it. This is durable, auditable, and cannot be undone by
selfHeal — the repository is the desired state.

### If the rollback needs a schema rollback too

**Usually it does not, and usually you should not.** Migrations here are
additive (new columns are nullable, new indexes are additive), so old code runs
fine against a newer schema. That is deliberate.

If you genuinely need to reverse one:

```bash
kubectl -n webhook-relay run migrate-down --rm -it --restart=Never \
  --image=migrate/migrate:v4.18.1 \
  --env="DATABASE_URL=$(kubectl -n webhook-relay get secret webhook-relay-db-credentials \
     -o jsonpath='{.data.password}' | base64 -d \
     | xargs -I{} echo "postgres://webhook_relay:{}@webhook-relay-db-rw:5432/webhook_relay?sslmode=disable")" \
  -- -path=/migrations -database="$DATABASE_URL" down 1
```

Reversing a migration that dropped data does not bring the data back.

---

## Climbing queue depth

**Alert:** `WebhookQueueDepthGrowing`, `WebhookBacklogAgeOverSLO`

### Decide which of three things it is

```bash
# 1. Are workers running at all?
kubectl -n webhook-relay get pods -l app.kubernetes.io/component=worker

# 2. Are they delivering, and to what effect?
kubectl -n webhook-relay logs -l app.kubernetes.io/component=worker --tail=100 \
  | grep -E 'delivered|failed|breaker|rate limited'

# 3. Is it one endpoint or everything?
```

In Grafana, look at **Delivery attempts by outcome**:

| What you see | What it means | What to do |
|---|---|---|
| Mostly `2xx`, depth still rising | Genuine overload — more work than capacity | Scale workers (below) |
| Mostly `5xx` | A receiver is down | Check whether its breaker opened; usually self-corrects |
| Mostly `error` | Network or DNS, our side | Check worker egress and CoreDNS |
| Almost no attempts at all | Workers are not consuming | See [nothing is being delivered](#nothing-is-being-delivered) |
| High `rate limited` | Endpoints are throttling us | Expected. Raise their limit only if the customer agrees |

### Scaling workers

```bash
kubectl -n webhook-relay scale deploy/webhook-relay-worker --replicas=6
```

> This is a **temporary** change. ArgoCD's selfHeal reverts it within ~30
> seconds. For a lasting change, edit `worker.replicaCount` in
> `deploy/charts/webhook-relay/values.yaml` and merge.
>
> To hold it during an incident, pause ArgoCD first:
>
> ```bash
> kubectl -n argocd patch application webhook-relay --type merge \
>   -p '{"spec":{"syncPolicy":{"automated":null}}}'
> ```
>
> **Write yourself a note to undo this.** A paused ArgoCD means no deployment
> works, and the symptom is "my merge did nothing" hours later.

**More workers does not always help.** Delivery is bounded by each endpoint's
own rate limit, so if the backlog is concentrated on a few throttled endpoints,
extra workers just queue on a token bucket. Check the **Rate limiting** panel
before scaling.

---

## Draining the DLQ

**Alert:** `WebhookDLQRateSpike`

An event reaches the DLQ two ways: it exhausted six attempts, or it failed
permanently (a 4xx that is not 408/429 — wrong URL, rejected signature).

### See what is in there

```bash
kubectl -n webhook-relay exec -it webhook-relay-db-1 -- \
  psql -U webhook_relay -d webhook_relay -c "
    SELECT ep.url,
           count(*)                                        AS events,
           max(e.created_at)                               AS most_recent
    FROM events e JOIN endpoints ep ON ep.id = e.endpoint_id
    WHERE e.status = 'dlq'
    GROUP BY ep.url ORDER BY events DESC LIMIT 20;"
```

### Find out why, before replaying

```bash
kubectl -n webhook-relay exec -it webhook-relay-db-1 -- \
  psql -U webhook_relay -d webhook_relay -c "
    SELECT da.status_code, da.error_message, count(*)
    FROM delivery_attempts da
    JOIN events e ON e.id = da.event_id
    WHERE e.status = 'dlq'
    GROUP BY 1, 2 ORDER BY 3 DESC LIMIT 10;"
```

**Fix the cause first.** Replaying into a broken endpoint just refills the DLQ
and burns the customer's rate limit doing it.

### Replay

```bash
API=http://webhook-relay.localhost:8081
curl -X POST "$API/v1/events/<event-id>/replay"
```

Replay resets the attempt budget but **keeps the failure history** — the
original attempts stay in `delivery_attempts` so you can still see what went
wrong.

Bulk replay for one endpoint:

```bash
# Get the ids, then replay them slowly enough not to re-trip the breaker.
kubectl -n webhook-relay exec webhook-relay-db-1 -- \
  psql -U webhook_relay -d webhook_relay -tAc \
  "SELECT id FROM events WHERE status='dlq' AND endpoint_id='<uuid>' LIMIT 100" \
| while read -r id; do
    curl -sS -X POST "$API/v1/events/$id/replay" >/dev/null
    sleep 0.2
  done
```

---

## An endpoint's breaker is open

**Alert:** `WebhookCircuitBreakerOpen`

**This is the system working, not a fault.** The endpoint failed 10 times in a
row, so we stopped calling it. It re-probes automatically after a 5-minute
cooldown and closes itself when the probe succeeds.

**Act only if it stays open**, which means the endpoint is genuinely down.

```bash
# Which endpoint, and what is its state
curl -sS http://webhook-relay.localhost:8081/v1/endpoints/<id> | jq \
  '{url, is_active, consecutive_failures, circuit_breaker_state}'
```

To force a probe immediately after the customer says they are fixed:

```bash
kubectl -n webhook-relay exec -it deploy/webhook-relay-worker -- true  # no-op; use redis-cli below
kubectl -n webhook-relay exec -it valkey-master-0 -- \
  valkey-cli DEL "breaker:endpoint:<endpoint-uuid>"
```

Events held by an open breaker are **not** consuming their retry budget — they
are deferred, not failed — so nothing is lost by waiting.

---

## Nothing is being delivered

**Alert:** `WebhookNoDeliveriesAttempted`

Queue is not empty, workers are up and healthy, and no attempts are happening.
This is the nastiest failure because everything looks green.

### 1. Did the consumer group vanish?

The most likely cause. If Valkey restarted without persistence, the stream and
its consumer group are gone.

```bash
kubectl -n webhook-relay exec -it valkey-master-0 -- \
  valkey-cli XINFO GROUPS webhook-relay:deliveries
```

If that errors with `no such key`, the group is gone. **The workers recover
from this on their own** — they detect `NOGROUP`, recreate the group, and
retry. If they are not recovering, check their logs for `NOGROUP` and restart
them:

```bash
kubectl -n webhook-relay rollout restart deploy/webhook-relay-worker
```

### 2. Are all the breakers open?

```bash
kubectl -n webhook-relay exec -it valkey-master-0 -- \
  valkey-cli --scan --pattern 'breaker:endpoint:*'
```

If every endpoint's breaker is open, the shared cause is usually ours, not
theirs — worker egress blocked, or DNS failing.

### 3. Is worker egress working?

```bash
kubectl -n webhook-relay exec -it deploy/webhook-relay-worker -- true
# Distroless: no shell. Use an ephemeral debug container instead:
kubectl -n webhook-relay debug -it deploy/webhook-relay-worker \
  --image=nicolaka/netshoot -- curl -sS -o /dev/null -w '%{http_code}\n' https://example.com
```

If that hangs, the NetworkPolicy or DNS is the problem.

---

## A pod is restarting

**Alert:** `WebhookPodRestartLoop`

```bash
kubectl -n webhook-relay describe pod <pod> | sed -n '/Events/,$p'
kubectl -n webhook-relay logs <pod> --previous
```

**Check the liveness probe first.** `/healthz` deliberately touches no
dependency. If a pod is being killed while its dependencies are down, someone
has pointed liveness at `/readyz` — undo that. A liveness probe that checks
Postgres kills every replica simultaneously during a database blip and turns a
recoverable outage into a CrashLoopBackOff that outlasts it.

**OOMKilled?**

```bash
kubectl -n webhook-relay get pod <pod> -o jsonpath='{.status.containerStatuses[0].lastState.terminated.reason}'
```

If so, raise `resources.limits.memory` in values.yaml and merge. Do not raise
it in the cluster — selfHeal reverts it.

---

## Postgres failover

**Alerts:** `PostgresReplicaLagHigh`, `PostgresClusterUnhealthy`

The cluster runs 3 instances managed by CloudNativePG. One is primary; the
others stream from it. **Failover is automatic** — the operator promotes a
standby and repoints the `-rw` service. The application connects to
`webhook-relay-db-rw`, which always points at the current primary, so it
follows the failover without a config change.

### See the current state

```bash
kubectl -n webhook-relay get cluster webhook-relay-db
kubectl -n webhook-relay get pods -l cnpg.io/cluster=webhook-relay-db \
  -L cnpg.io/instanceRole
```

`cnpg.io/instanceRole=primary` marks the current primary.

### Watch a failover

```bash
# Delete the primary and watch a standby get promoted
kubectl -n webhook-relay delete pod <primary-pod>
kubectl -n webhook-relay get pods -l cnpg.io/cluster=webhook-relay-db \
  -L cnpg.io/instanceRole -w
```

Expect a few seconds of write errors in the app logs while the service
repoints. Events being ingested during that window get a 500 and the client
retries; events already stored are untouched.

### If lag is high

A failover now would promote a standby missing that much data.

```bash
kubectl -n webhook-relay exec -it webhook-relay-db-1 -- \
  psql -U postgres -c "SELECT client_addr, state, sent_lsn, replay_lsn,
    (pg_current_wal_lsn() - replay_lsn) AS lag_bytes FROM pg_stat_replication;"
```

Usually the standby is IO-starved. On this cluster that means the node is
under memory pressure — check `kubectl top nodes`.

### Manual switchover (planned, not an emergency)

```bash
kubectl -n webhook-relay patch cluster webhook-relay-db --type merge \
  -p '{"spec":{"switchoverDelay":30}}'
kubectl cnpg promote webhook-relay-db webhook-relay-db-2   # needs the cnpg plugin
```

---

## Recovering the whole cluster

```bash
make cluster-down
make cluster-up
make bootstrap
```

**Before ArgoCD syncs**, restore the Sealed Secrets key or every committed
`SealedSecret` becomes undecryptable:

```bash
kubectl apply -f sealed-secrets-key.yaml
kubectl -n kube-system delete pod -l name=sealed-secrets-controller
```

Postgres data does **not** survive `cluster-down` — the PVCs go with the
cluster. That is fine here; in a real deployment this is what backups are for,
and CloudNativePG supports continuous WAL archiving to object storage.

---

## Known gaps you may hit

- **NetworkPolicies are not enforced.** k3s ships Flannel, which accepts the
  objects and ignores them. Real enforcement needs Calico or Cilium. The
  policies are still correct and committed; they just are not doing anything on
  this cluster.
- **`kubectl edit` never lasts.** ArgoCD selfHeal reverts anything you change
  by hand within ~30 seconds. Change values.yaml and merge, or pause ArgoCD
  (and remember to unpause).
- **The HPA's queue-depth metric is disabled by default.** It needs
  prometheus-adapter, which is not installed. The CPU target still works.
