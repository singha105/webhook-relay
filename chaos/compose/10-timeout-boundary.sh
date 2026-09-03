#!/usr/bin/env bash
#
# Experiment 10: a receiver that answers 200 after 9.9s, against a 10s timeout.
#
# The nastiest case in the set, because it is not a failure — it is a SUCCESS
# that arrives 100ms before we give up.
#
# PREDICTION
# Some deliveries time out. A timeout is classified retryable with a NIL status
# code, so those events are retried — which means the receiver, which already
# processed them successfully, processes them AGAIN.
#
# The dedup guard does NOT help. It suppresses a duplicate dispatch of the same
# ATTEMPT; a timeout produces a legitimate NEXT attempt with a new number. Only
# the receiver deduplicating on X-Webhook-Id prevents double processing.
#
# So the expected evidence is: sink received MORE deliveries than there are
# events, while our own records show each event delivered once.
set -euo pipefail
API="${API_URL:-http://localhost:8090}"
SINK_CTL="${SINK_URL:-http://localhost:9091}"
SINK_TARGET="${SINK_TARGET:-http://sink:9090/hook}"
COMPOSE="docker compose -f deploy/compose/docker-compose.yml --env-file .env"
EVENTS="${EVENTS:-30}"
DELAY="${DELAY:-9.9s}"
command -v jq >/dev/null || { echo "need jq" >&2; exit 1; }
PSQL() { docker exec "$($COMPOSE ps -q postgres)" psql -U webhook_relay -d webhook_relay -tAc "$1"; }

PSQL "TRUNCATE endpoints CASCADE;" >/dev/null
curl -fsS -X POST "$SINK_CTL/_control/reset" >/dev/null
curl -fsS -X POST "$SINK_CTL/_control/behavior" -H 'Content-Type: application/json' \
  -d "{\"status\":200,\"delay\":\"${DELAY}\"}" >/dev/null
echo "  sink responds 200 after ${DELAY}; client timeout is 10s"

EP=$(curl -fsS -X POST "$API/v1/endpoints" -H 'Content-Type: application/json' \
      -d "{\"url\":\"${SINK_TARGET}\",\"description\":\"timeout-boundary\",\"rate_limit_per_sec\":1000}" | jq -r .id)

echo "  posting ${EVENTS} events"
for i in $(seq 1 "$EVENTS"); do
  curl -fsS -o /dev/null -X POST "$API/v1/events" -H 'Content-Type: application/json' \
    -d "{\"endpoint_id\":\"${EP}\",\"event_type\":\"boundary\",\"payload\":{\"n\":${i}}}" &
done
wait

echo "  waiting for the pipeline to settle (this is slow by construction)..."
DEADLINE=$(( $(date +%s) + 420 ))
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  REMAIN=$(PSQL "SELECT count(*) FROM events WHERE status NOT IN ('delivered','dlq');")
  [ "$REMAIN" = "0" ] && break
  sleep 5
done

DELIVERED=$(PSQL "SELECT count(*) FROM events WHERE status='delivered';")
DLQ=$(PSQL "SELECT count(*) FROM events WHERE status='dlq';")
ATTEMPTS=$(PSQL "SELECT count(*) FROM delivery_attempts;")
TIMEOUTS=$(PSQL "SELECT count(*) FROM delivery_attempts WHERE status_code IS NULL;")
SINK_TOTAL=$(curl -fsS "$SINK_CTL/_control/stats" | jq -r .total)
SINK_DISTINCT=$(curl -fsS "$SINK_CTL/_control/stats" | jq -r .distinct_events)

echo ""
echo "  events posted:                    ${EVENTS}"
echo "  our records: delivered            ${DELIVERED}"
echo "  our records: dead-lettered        ${DLQ}"
echo "  our records: total attempts       ${ATTEMPTS}"
echo "  our records: timeouts (null code) ${TIMEOUTS}"
echo "  the RECEIVER actually processed   ${SINK_TOTAL}   (distinct events: ${SINK_DISTINCT})"
echo ""
EXTRA=$(( SINK_TOTAL - EVENTS ))
echo "  double-processed by the receiver: ${EXTRA}"
echo ""
echo "  Every one of those is a delivery we timed out on, retried, and the"
echo "  receiver had already handled. This is the at-least-once contract"
echo "  meeting reality; only receiver-side dedup on X-Webhook-Id fixes it."

curl -fsS -X POST "$SINK_CTL/_control/behavior" -H 'Content-Type: application/json' -d '{"status":200}' >/dev/null
