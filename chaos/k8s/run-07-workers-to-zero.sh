#!/usr/bin/env bash
#
# Experiment 7 runner: workers repeatedly killed for ~3 minutes while ingest
# continues, then recovery.
#
# The prediction is in chaos/07-scale-workers-to-zero.yaml. This script only
# measures; it does not decide what the answer should be.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
NS=webhook-relay
DURATION="${DURATION:-180}"
RATE="${RATE:-5}"

PSQL() { kubectl exec -n "$NS" webhook-relay-db-1 -c postgres -- psql -U postgres -d webhook_relay -tAc "$1" 2>/dev/null | tr -d '[:space:]'; }
ts() { printf '  [%s] %s\n' "$(date -u +%H:%M:%SZ)" "$*"; }

kubectl port-forward -n "$NS" svc/webhook-relay-api 18080:80 >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true' EXIT
sleep 6

EP=$(curl -fsS -X POST http://localhost:18080/v1/endpoints -H 'Content-Type: application/json' \
  -d '{"url":"http://webhook-relay-sink.webhook-relay.svc.cluster.local:9090/hook","description":"exp7","rate_limit_per_sec":1000}' | jq -r .id)
ts "endpoint $EP"

ts "applying the chaos schedule (pod-kill, mode=all, every 30s)"
kubectl apply -f chaos/07-scale-workers-to-zero.yaml >/dev/null

posted=0; errors=0
END=$(( $(date +%s) + DURATION ))
while [ "$(date +%s)" -lt "$END" ]; do
  for _ in $(seq 1 "$RATE"); do
    if curl -fsS -o /dev/null -m 5 -X POST http://localhost:18080/v1/events \
        -H 'Content-Type: application/json' \
        -d "{\"endpoint_id\":\"${EP}\",\"event_type\":\"exp7\",\"payload\":{\"n\":${posted}}}" 2>/dev/null; then
      posted=$(( posted + 1 ))
    else
      errors=$(( errors + 1 ))
    fi
  done
  if (( posted % 100 < RATE )); then
    ready=$(kubectl get pods -n "$NS" -l app.kubernetes.io/component=api \
      -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{" "}{end}' 2>/dev/null)
    ts "posted=${posted} ingest_errors=${errors} pending=$(PSQL "SELECT count(*) FROM events WHERE event_type='exp7' AND status NOT IN ('delivered','dlq');") api_ready=[${ready}]"
  fi
  sleep 1
done

ts "removing the chaos schedule"
kubectl delete -f chaos/07-scale-workers-to-zero.yaml >/dev/null 2>&1 || true
kubectl delete podchaos -n chaos-testing --all >/dev/null 2>&1 || true

ts "waiting for the backlog to drain"
DEADLINE=$(( $(date +%s) + 420 ))
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  out=$(PSQL "SELECT count(*) FROM events WHERE event_type='exp7' AND status NOT IN ('delivered','dlq');")
  [ "${out:-1}" = "0" ] && break
  sleep 10
done

delivered=$(PSQL "SELECT count(*) FROM events WHERE event_type='exp7' AND status='delivered';")
dlq=$(PSQL "SELECT count(*) FROM events WHERE event_type='exp7' AND status='dlq';")
total=$(PSQL "SELECT count(*) FROM events WHERE event_type='exp7';")
echo ""
echo "  events accepted by the API:   ${posted}"
echo "  ingest errors during chaos:   ${errors}"
echo "  rows in the database:         ${total}"
echo "  delivered:                    ${delivered}"
echo "  dead-lettered:                ${dlq}"
echo "  LOST (accepted, no row):      $(( posted - total ))"
