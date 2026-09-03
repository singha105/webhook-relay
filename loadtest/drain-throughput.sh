#!/usr/bin/env bash
#
# Measures sustained DELIVERY throughput, which is what the ramp showed to be
# the constraint — ingest reached 875/s while delivery plateaued around 330/s.
#
# Method: post N events as fast as possible, then time how long the pipeline
# takes to drive every one of them to a terminal state. Wall-clock divided by N
# is delivery throughput, with no dependence on how fast the load generator
# offers work.
#
# This is the script the before/after optimisation numbers come from. Run it
# identically on both sides; the only thing that changes between runs is the
# configuration under test.
#
#   ./loadtest/drain-throughput.sh              # default 5000 events
#   EVENTS=10000 ./loadtest/drain-throughput.sh
#
set -euo pipefail

API="${API_URL:-http://localhost:8090}"
SINK_CTL="${SINK_URL:-http://localhost:9091}"
SINK_TARGET="${SINK_TARGET:-http://sink:9090/hook}"
EVENTS="${EVENTS:-5000}"
LABEL="${LABEL:-run}"
COMPOSE="docker compose -f deploy/compose/docker-compose.yml --env-file .env"

command -v jq >/dev/null || { echo "need jq" >&2; exit 1; }

PSQL() { docker exec "$($COMPOSE ps -q postgres)" psql -U webhook_relay -d webhook_relay -tAc "$1"; }

# A clean slate every run, or the previous run's backlog is counted as this
# run's throughput.
PSQL "TRUNCATE endpoints CASCADE;" >/dev/null
curl -fsS -X POST "$SINK_CTL/_control/reset" >/dev/null
curl -fsS -X POST "$SINK_CTL/_control/behavior" -H 'Content-Type: application/json' \
  -d '{"status":200}' >/dev/null

EP=$(curl -fsS -X POST "$API/v1/endpoints" -H 'Content-Type: application/json' \
      -d "{\"url\":\"${SINK_TARGET}\",\"description\":\"throughput ${LABEL}\",\"rate_limit_per_sec\":1000}" \
    | jq -r .id)

echo "  label:      ${LABEL}"
echo "  events:     ${EVENTS}"
echo "  endpoint:   ${EP}"

# STOP THE WORKERS before posting.
#
# Without this the measurement is confounded: delivery runs concurrently with
# the posting phase, so most events are already delivered by the time posting
# finishes and the timed "drain" only measures the tail. A first version of
# this script did exactly that and reported 1562/s, when the ramp had just
# shown sustained delivery plateauing near 330/s.
#
# With workers stopped, the full backlog exists before the clock starts, so
# drain time over event count is delivery throughput and nothing else.
echo -n "  stopping workers... "
$COMPOSE stop worker >/dev/null 2>&1
echo "done"

echo -n "  posting... "
POST_START=$(python3 -c 'import time;print(time.time())')
for i in $(seq 1 "$EVENTS"); do
  curl -fsS -o /dev/null -X POST "$API/v1/events" -H 'Content-Type: application/json' \
    -d "{\"endpoint_id\":\"${EP}\",\"event_type\":\"throughput\",\"payload\":{\"n\":${i}}}" &
  (( i % 60 == 0 )) && wait
done
wait
POST_END=$(python3 -c 'import time;print(time.time())')
POST_SECS=$(python3 -c "print(round(${POST_END}-${POST_START},1))")
echo "done in ${POST_SECS}s ($(python3 -c "print(round(${EVENTS}/${POST_SECS}))")/s ingest)"

BACKLOG=$(PSQL "SELECT count(*) FROM events WHERE status NOT IN ('delivered','dlq');")
echo "  backlog before workers start: ${BACKLOG}"

echo -n "  starting workers and draining... "
DRAIN_START=$(python3 -c 'import time;print(time.time())')
$COMPOSE start worker >/dev/null 2>&1
DEADLINE=$(( $(date +%s) + 900 ))
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  REMAINING=$(PSQL "SELECT count(*) FROM events WHERE status NOT IN ('delivered','dlq');")
  [ "$REMAINING" = "0" ] && break
  sleep 2
done
DRAIN_END=$(python3 -c 'import time;print(time.time())')
DRAIN_SECS=$(python3 -c "print(round(${DRAIN_END}-${DRAIN_START},1))")

DELIVERED=$(PSQL "SELECT count(*) FROM events WHERE status='delivered';")
DLQ=$(PSQL "SELECT count(*) FROM events WHERE status='dlq';")
SINK_TOTAL=$(curl -fsS "$SINK_CTL/_control/stats" | jq -r '.total')
DUPES=$(curl -fsS "$SINK_CTL/_control/stats" | jq -r '.duplicate_sends | length')
RATE=$(python3 -c "print(round(${DELIVERED}/max(${DRAIN_SECS},0.1),1))")

echo "done"
echo ""
echo "  backlog at start:    ${BACKLOG}"
echo "  ingest:              ${POST_SECS}s"
echo "  drain:               ${DRAIN_SECS}s"
echo "  delivered:           ${DELIVERED}"
echo "  dead-lettered:       ${DLQ}"
echo "  sink received:       ${SINK_TOTAL}"
echo "  duplicate dispatch:  ${DUPES}"
echo "  DELIVERY THROUGHPUT: ${RATE} events/sec"
echo ""

mkdir -p loadtest/results
cat > "loadtest/results/throughput-${LABEL}.json" <<JSON
{
  "label": "${LABEL}",
  "events": ${EVENTS},
  "backlog_at_drain_start": ${BACKLOG},
  "ingest_seconds": ${POST_SECS},
  "drain_seconds": ${DRAIN_SECS},
  "delivered": ${DELIVERED},
  "dead_lettered": ${DLQ},
  "sink_received": ${SINK_TOTAL},
  "duplicate_dispatches": ${DUPES},
  "delivery_throughput_per_sec": ${RATE}
}
JSON
echo "  wrote loadtest/results/throughput-${LABEL}.json"
