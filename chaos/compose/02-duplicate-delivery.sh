#!/usr/bin/env bash
#
# Experiment 2, on the Compose stack: prove the delivery-side dedup guard is
# load-bearing.
#
# This is the postmortem. It runs the SAME scenario twice — kill a worker
# mid-delivery under load — once with the guard off and once with it on, and
# compares the sink's record of what it actually received.
#
# The Kubernetes version is chaos/02-kill-worker-dedup-disabled.yaml. This
# exists because an experiment that only runs on infrastructure the reader
# cannot start is one they have to take on trust.
#
#   ./chaos/compose/02-duplicate-delivery.sh
#
set -euo pipefail

# Timestamped output. The postmortem needs a real timeline, and a timeline
# reconstructed after the fact is a guess with punctuation.
ts() { printf '  [%s] %s\n' "$(date -u '+%H:%M:%SZ')" "$*"; }

API="${API_URL:-http://localhost:8090}"
SINK_CTL="${SINK_URL:-http://localhost:9091}"
SINK_TARGET="${SINK_TARGET:-http://sink:9090/hook}"
COMPOSE="docker compose -f deploy/compose/docker-compose.yml --env-file .env"

# Long enough that a kill lands while requests are genuinely in flight.
EVENTS="${EVENTS:-400}"
# The sink holds each request open this long, which is what creates the window
# between "request sent" and "response received" that a kill can land inside.
HOLD="${HOLD:-2s}"

need() { command -v "$1" >/dev/null || { echo "need $1" >&2; exit 1; }; }
need curl; need jq

hr() { printf '%s\n' "------------------------------------------------------------"; }

wait_ready() {
  for _ in $(seq 1 60); do
    curl -fsS "$API/readyz" >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "api never became ready" >&2; exit 1
}

# run_once <dedup_enabled> <label>
# Returns the number of duplicate (event_id, attempt) pairs the sink saw.
run_once() {
  local dedup="$1" label="$2"

  hr
  echo "  ${label}  (DELIVERY_DEDUP_ENABLED=${dedup})"
  hr

  # Restart the worker with the flag set. --no-deps so Postgres and Valkey keep
  # their state; only the worker's configuration changes between the two runs.
  DELIVERY_DEDUP_ENABLED="$dedup" $COMPOSE up -d --no-deps worker >/dev/null 2>&1
  sleep 6
  wait_ready

  curl -fsS -X POST "$SINK_CTL/_control/reset" >/dev/null
  # Hold each request open, so a kill lands between send and ack.
  curl -fsS -X POST "$SINK_CTL/_control/behavior" -H 'Content-Type: application/json' \
    -d "{\"status\":200,\"delay\":\"${HOLD}\"}" >/dev/null

  local ep
  ep=$(curl -fsS -X POST "$API/v1/endpoints" -H 'Content-Type: application/json' \
        -d "{\"url\":\"${SINK_TARGET}\",\"description\":\"dup-test-${dedup}\",\"rate_limit_per_sec\":1000}" \
      | jq -r .id)
  echo "  endpoint ${ep}"

  ts "posting ${EVENTS} events"
  for i in $(seq 1 "$EVENTS"); do
    curl -fsS -o /dev/null -X POST "$API/v1/events" -H 'Content-Type: application/json' \
      -d "{\"endpoint_id\":\"${ep}\",\"event_type\":\"dup.test\",\"payload\":{\"n\":${i}}}" &
    (( i % 50 == 0 )) && wait
  done
  wait

  # Wait until deliveries are genuinely in flight before killing.
  for _ in $(seq 1 30); do
    [ "$(curl -fsS "$SINK_CTL/_control/stats" | jq -r '.in_flight')" -gt 0 ] && break
    sleep 0.5
  done
  ts "in flight at kill time: $(curl -fsS "$SINK_CTL/_control/stats" | jq -r '.in_flight')"

  ts "KILL -9 the worker"
  # SIGKILL, not SIGTERM: a graceful stop drains, which is the case Day 3
  # already covers. This is the ungraceful one.
  docker kill --signal=KILL "$($COMPOSE ps -q worker)" >/dev/null 2>&1 || true
  sleep 2
  $COMPOSE up -d --no-deps worker >/dev/null 2>&1
  ts "worker restarted; waiting for the stale-claim reclaim"

  # The reclaim only happens after STALE_CLAIM_TIMEOUT (60s by default), so
  # this genuinely has to wait it out.
  local deadline=$(( $(date +%s) + 180 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    local pending
    pending=$(docker exec "$($COMPOSE ps -q postgres)" psql -U webhook_relay -d webhook_relay -tAc \
      "SELECT count(*) FROM events WHERE endpoint_id='${ep}' AND status NOT IN ('delivered','dlq');" 2>/dev/null || echo 1)
    [ "$pending" = "0" ] && break
    sleep 5
  done

  local stats dupes total distinct
  stats=$(curl -fsS "$SINK_CTL/_control/stats")
  total=$(echo "$stats" | jq -r '.total')
  distinct=$(echo "$stats" | jq -r '.distinct_events')
  dupes=$(echo "$stats" | jq -r '.duplicate_sends | length')

  ts "all events reached a terminal state"
  echo ""
  echo "  events posted:                 ${EVENTS}"
  echo "  deliveries the sink received:  ${total}"
  echo "  distinct events at the sink:   ${distinct}"
  echo "  DUPLICATE (event, attempt):    ${dupes}"
  if [ "$dupes" -gt 0 ]; then
    echo ""
    echo "  the duplicated pairs:"
    echo "$stats" | jq -r '.duplicate_sends | to_entries[] | "    \(.key) delivered \(.value) times"' | head -10
  fi
  echo ""

  echo "$dupes" > "/tmp/dupes_${dedup}"
}

wait_ready
run_once false "RUN 1 of 2 - dedup DISABLED"
run_once true  "RUN 2 of 2 - dedup ENABLED"

WITHOUT=$(cat /tmp/dupes_false)
WITH=$(cat /tmp/dupes_true)

hr
echo "  RESULT"
hr
printf '  dedup disabled -> %s duplicate (event, attempt) pairs\n' "$WITHOUT"
printf '  dedup enabled  -> %s duplicate (event, attempt) pairs\n' "$WITH"
echo ""
if [ "$WITHOUT" -gt 0 ] && [ "$WITH" -eq 0 ]; then
  echo "  The guard is load-bearing: duplicates appear without it and vanish with it."
elif [ "$WITHOUT" -eq 0 ]; then
  echo "  No duplicates even with the guard off. The kill did not land between"
  echo "  send and ack — increase HOLD or EVENTS and re-run."
else
  echo "  Duplicates present WITH the guard enabled. That is a real bug."
fi

# Leave the sink as we found it.
curl -fsS -X POST "$SINK_CTL/_control/behavior" -H 'Content-Type: application/json' \
  -d '{"status":200}' >/dev/null
