#!/usr/bin/env bash
#
# Experiment 11: what actually happens to an event the dedup guard suppresses?
#
# Experiment 2 measures a ~3 minute window and reports 0 duplicates with the
# guard enabled. This asks the follow-up question that window is too short to
# answer: the suppressed events are left in 'delivering' with no recorded
# outcome -- do they ever resolve, and if so, how?
#
# PREDICTION
#   The guard does not prevent the duplicate, it DELAYS it. The event cycles
#   (lease expires -> relay re-enqueues -> guard suppresses -> ack -> stranded)
#   until the dedup key's TTL expires, and is then delivered a second time.
#   So over a window longer than DELIVERY_DEDUP_TTL, duplicates > 0.
#
# WHAT WOULD FALSIFY IT
#   The event reaching a terminal state without a second delivery, or staying
#   in 'delivering' forever with no duplicate ever appearing.
#
# Timings are compressed (short dedup TTL and lease) so the full cycle is
# observable in minutes instead of the 15 the defaults imply. The MECHANISM is
# unchanged -- only the clock is.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

API="${API_URL:-http://localhost:8090}"
SINK_CTL="${SINK_URL:-http://localhost:9091}"
SINK_TARGET="${SINK_TARGET:-http://sink:9090/hook}"
COMPOSE=(docker compose -f deploy/compose/docker-compose.yml --env-file .env --profile demo --profile tools)
EVENTS="${EVENTS:-40}"
ts() { printf '  [%s] %s\n' "$(date -u '+%H:%M:%SZ')" "$*"; }
PSQL() { docker exec "$("${COMPOSE[@]}" ps -q postgres)" psql -U webhook_relay -d webhook_relay -tAc "$1" 2>/dev/null | tr -d '[:space:]'; }

# Compressed clock: dedup TTL 60s, stale claim 20s, lease 45s.
export DELIVERY_DEDUP_TTL=60s STALE_CLAIM_TIMEOUT=20s DELIVERY_LEASE=45s
"${COMPOSE[@]}" up -d --force-recreate worker >/dev/null 2>&1
sleep 6
# Verify the compressed clock actually applied. Three experiments across Day 5
# and Day 6 produced numbers that were not measuring what they claimed because
# an env var was never plumbed through docker-compose. Assert, do not assume.
CFG=$(docker logs "$("${COMPOSE[@]}" ps -q worker)" 2>&1 | grep -o '"delivery_dedup_ttl":"[^"]*"' | tail -1)
STALE=$(docker logs "$("${COMPOSE[@]}" ps -q worker)" 2>&1 | grep -o '"stale_claim_timeout":"[^"]*"' | tail -1)
ts "worker config: ${CFG}, ${STALE}"
case "$CFG" in
  *15m0s*) echo "  ABORT: the compressed dedup TTL did not apply -- this run would measure the defaults." >&2; exit 1 ;;
esac

PSQL "TRUNCATE endpoints CASCADE;" >/dev/null
curl -fsS -X POST "$SINK_CTL/_control/reset" >/dev/null
curl -fsS -X POST "$SINK_CTL/_control/behavior" -H 'Content-Type: application/json' -d '{"status":200,"delay":"3s"}' >/dev/null
EP=$(curl -fsS -X POST "$API/v1/endpoints" -H 'Content-Type: application/json' \
      -d "{\"url\":\"${SINK_TARGET}\",\"description\":\"stranded\",\"rate_limit_per_sec\":1000}" | jq -r .id)

ts "posting ${EVENTS} events"
for i in $(seq 1 "$EVENTS"); do
  curl -fsS -o /dev/null -X POST "$API/v1/events" -H 'Content-Type: application/json' \
    -d "{\"endpoint_id\":\"${EP}\",\"event_type\":\"stranded\",\"payload\":{\"n\":${i}}}" &
done
wait

for _ in $(seq 1 40); do
  inflight=$(curl -fsS "$SINK_CTL/_control/stats" | jq -r '.in_flight')
  [ "${inflight:-0}" -gt 0 ] && break
  sleep 0.5
done
ts "in flight: ${inflight}"
docker kill --signal=KILL "$("${COMPOSE[@]}" ps -q worker)" >/dev/null 2>&1 || true
ts "KILL -9"
"${COMPOSE[@]}" up -d worker >/dev/null 2>&1

echo ""
printf '  %-10s %-12s %-12s %-10s %s\n' "time" "delivering" "delivered" "sink" "dups"
for i in $(seq 1 30); do
  dlv=$(PSQL "SELECT count(*) FROM events WHERE status='delivered';")
  dlving=$(PSQL "SELECT count(*) FROM events WHERE status='delivering';")
  st=$(curl -fsS "$SINK_CTL/_control/stats")
  printf '  %-10s %-12s %-12s %-10s %s\n' "$(date -u +%H:%M:%SZ)" "${dlving:-?}" "${dlv:-?}" "$(jq -r .total <<<"$st")" "$(jq -r '.duplicate_sends|length' <<<"$st")"
  sleep 10
done

echo ""
DUPS=$(curl -fsS "$SINK_CTL/_control/stats" | jq -r '.duplicate_sends | length')
STRANDED=$(PSQL "SELECT count(*) FROM events WHERE status='delivering';")
echo "  final: ${DUPS} duplicate (event, attempt) pairs, ${STRANDED} still stranded"
if [ "${DUPS:-0}" -gt 0 ]; then
  echo "  PREDICTION CONFIRMED: the guard delayed the duplicate, it did not prevent it."
else
  echo "  PREDICTION NOT CONFIRMED in this window."
fi
