#!/usr/bin/env bash
# Experiment 4 runner. Prediction lives in chaos/04-network-latency-to-sink.yaml.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
NS=webhook-relay
PSQL() { kubectl exec -n "$NS" webhook-relay-db-1 -c postgres -- psql -U postgres -d webhook_relay -tAc "$1" 2>/dev/null | tr -d '[:space:]'; }
ts() { printf '  [%s] %s\n' "$(date -u +%H:%M:%SZ)" "$*"; }

kubectl port-forward -n "$NS" svc/webhook-relay-api 18080:80 >/dev/null 2>&1 &
PF=$!; trap 'kill $PF 2>/dev/null || true' EXIT; sleep 6

EP=$(curl -fsS -X POST http://localhost:18080/v1/endpoints -H 'Content-Type: application/json' \
  -d '{"url":"http://webhook-relay-sink.webhook-relay.svc.cluster.local:9090/hook","description":"exp4","rate_limit_per_sec":1000}' | jq -r .id)
ts "endpoint $EP"

ts "injecting 30s latency toward the receiver (client timeout is 10s)"
kubectl apply -f chaos/04-network-latency-to-sink.yaml >/dev/null
sleep 5

for i in $(seq 1 40); do
  curl -fsS -o /dev/null -m 5 -X POST http://localhost:18080/v1/events -H 'Content-Type: application/json' \
    -d "{\"endpoint_id\":\"${EP}\",\"event_type\":\"exp4\",\"payload\":{\"n\":${i}}}" 2>/dev/null || true
done
ts "posted 40 events"

for i in $(seq 1 10); do
  sleep 30
  br=$(curl -fsS "http://localhost:18080/v1/endpoints/${EP}" 2>/dev/null | jq -r '.consecutive_failures // 0')
  ts "t+$((i*30))s states: $(PSQL "SELECT string_agg(status||'='||c,' ') FROM (SELECT status, count(*) c FROM events WHERE event_type='exp4' GROUP BY status) x;") consecutive_failures=${br}"
done

ts "removing the latency"
kubectl delete -f chaos/04-network-latency-to-sink.yaml >/dev/null 2>&1 || true
kubectl delete networkchaos -n chaos-testing --all >/dev/null 2>&1 || true

ts "waiting for recovery"
for i in $(seq 1 30); do
  out=$(PSQL "SELECT count(*) FROM events WHERE event_type='exp4' AND status NOT IN ('delivered','dlq');")
  [ "${out:-1}" = "0" ] && break
  sleep 20
done
echo ""
echo "  final: $(PSQL "SELECT string_agg(status||'='||c,' ') FROM (SELECT status, count(*) c FROM events WHERE event_type='exp4' GROUP BY status) x;")"
echo "  attempts recorded:   $(PSQL "SELECT count(*) FROM delivery_attempts a JOIN events e ON e.id=a.event_id WHERE e.event_type='exp4';")"
echo "  timeouts (null code):$(PSQL "SELECT count(*) FROM delivery_attempts a JOIN events e ON e.id=a.event_id WHERE e.event_type='exp4' AND a.status_code IS NULL;")"
