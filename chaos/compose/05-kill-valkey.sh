#!/usr/bin/env bash
#
# Experiment 5 on the Compose stack: kill Valkey and see what survives.
#
# PREDICTION
# Valkey runs with NO persistence, so killing it loses the queue, the consumer
# group, the rate-limit buckets, the breaker state and the dedup keys.
#
# Expected: workers hit NOGROUP, RECREATE the group, and carry on — that is the
# Day 3 fix. The events themselves are untouched because Postgres is the system
# of record; they sit in 'delivering' with a lease, and when it expires the
# outbox relay requeues them. Nothing lost, some events delayed by up to one
# lease period.
set -euo pipefail
API="${API_URL:-http://localhost:8090}"
SINK_CTL="${SINK_URL:-http://localhost:9091}"
SINK_TARGET="${SINK_TARGET:-http://sink:9090/hook}"
COMPOSE="docker compose -f deploy/compose/docker-compose.yml --env-file .env"
EVENTS="${EVENTS:-200}"
command -v jq >/dev/null || { echo "need jq" >&2; exit 1; }
PSQL() { docker exec "$($COMPOSE ps -q postgres)" psql -U webhook_relay -d webhook_relay -tAc "$1"; }

PSQL "TRUNCATE endpoints CASCADE;" >/dev/null
curl -fsS -X POST "$SINK_CTL/_control/reset" >/dev/null
curl -fsS -X POST "$SINK_CTL/_control/behavior" -H 'Content-Type: application/json' \
  -d '{"status":200,"delay":"1s"}' >/dev/null

EP=$(curl -fsS -X POST "$API/v1/endpoints" -H 'Content-Type: application/json' \
      -d "{\"url\":\"${SINK_TARGET}\",\"description\":\"valkey-kill\",\"rate_limit_per_sec\":1000}" | jq -r .id)

echo "  posting ${EVENTS} events"
for i in $(seq 1 "$EVENTS"); do
  curl -fsS -o /dev/null -X POST "$API/v1/events" -H 'Content-Type: application/json' \
    -d "{\"endpoint_id\":\"${EP}\",\"event_type\":\"vk\",\"payload\":{\"n\":${i}}}" &
  (( i % 50 == 0 )) && wait
done
wait

sleep 3
echo "  before the kill:"
PSQL "SELECT '    ' || status || ': ' || count(*) FROM events GROUP BY status ORDER BY 1;"
echo "    stream length: $(docker exec "$($COMPOSE ps -q valkey)" valkey-cli XLEN webhook-relay:deliveries 2>/dev/null || echo '?')"

echo "  KILL -9 valkey"
docker kill --signal=KILL "$($COMPOSE ps -q valkey)" >/dev/null 2>&1 || true
sleep 3
$COMPOSE up -d --no-deps valkey >/dev/null 2>&1
echo "  valkey restarted (empty)"

echo "  waiting up to 6 minutes for the system to drain on its own..."
DEADLINE=$(( $(date +%s) + 400 ))
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  REMAIN=$(PSQL "SELECT count(*) FROM events WHERE status NOT IN ('delivered','dlq');")
  [ "$REMAIN" = "0" ] && break
  sleep 10
done

echo ""
echo "  after recovery:"
PSQL "SELECT '    ' || status || ': ' || count(*) FROM events GROUP BY status ORDER BY 1;"
echo "    sink received: $(curl -fsS "$SINK_CTL/_control/stats" | jq -r .total)"
echo "    duplicates:    $(curl -fsS "$SINK_CTL/_control/stats" | jq -r '.duplicate_sends | length')"
echo "    consumer group recreated: $(docker exec "$($COMPOSE ps -q valkey)" valkey-cli XINFO GROUPS webhook-relay:deliveries 2>/dev/null | head -2 | tail -1 || echo 'absent')"
echo ""
echo "  Events lost = ${EVENTS} minus (delivered + dlq)."
curl -fsS -X POST "$SINK_CTL/_control/behavior" -H 'Content-Type: application/json' -d '{"status":200}' >/dev/null
