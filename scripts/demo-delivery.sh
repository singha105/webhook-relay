#!/usr/bin/env bash
#
# The Day 2 verification, end to end:
#   1. sink returns 500  -> six attempts with widening backoff -> status dlq
#   2. sink returns 200  -> replay -> delivered
#   3. verify a signature by hand with the /pkg helper
#
# Requires the demo stack: make demo-up
set -euo pipefail

API_URL="${API_URL:-http://localhost:8090}"
SINK_PORT="${SINK_PORT:-9090}"
SINK_CONTROL="http://localhost:${SINK_PORT}"
# The worker reaches the sink by compose service name, not via the host port.
SINK_TARGET="${SINK_TARGET:-http://sink:9090/hook}"

for cmd in curl jq; do
  command -v "$cmd" >/dev/null || { echo "demo: $cmd is required" >&2; exit 1; }
done
curl -fsS "${API_URL}/readyz" >/dev/null 2>&1 || { echo "demo: api not ready at ${API_URL} — run 'make demo-up'" >&2; exit 1; }
curl -fsS "${SINK_CONTROL}/healthz" >/dev/null 2>&1 || { echo "demo: sink not ready at ${SINK_CONTROL} — run 'make demo-up'" >&2; exit 1; }

hr() { printf '%s\n' "------------------------------------------------------------"; }

curl -fsS -X POST "${SINK_CONTROL}/_control/reset" >/dev/null
curl -fsS -X POST "${SINK_CONTROL}/_control/behavior" \
  -H 'Content-Type: application/json' -d '{"status":500}' >/dev/null

hr
echo "1. sink is returning 500 for every delivery"
hr

endpoint=$(curl -fsS -X POST "${API_URL}/v1/endpoints" \
  -H 'Content-Type: application/json' \
  -d "{\"url\":\"${SINK_TARGET}\",\"description\":\"day-2 demo\"}")
endpoint_id=$(echo "$endpoint" | jq -r .id)
secret=$(echo "$endpoint" | jq -r .signing_secret)
echo "   endpoint ${endpoint_id}"

event=$(curl -fsS -X POST "${API_URL}/v1/events" \
  -H 'Content-Type: application/json' \
  -d "{\"endpoint_id\":\"${endpoint_id}\",\"event_type\":\"order.created\",\"payload\":{\"order_id\":\"ord_demo\",\"amount\":4999}}")
event_id=$(echo "$event" | jq -r .id)
echo "   event    ${event_id}  (status $(echo "$event" | jq -r .status))"
echo ""
echo "   waiting for the retry chain to exhaust..."

deadline=$(( $(date +%s) + 180 ))
status=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  status=$(curl -fsS "${API_URL}/v1/events/${event_id}" | jq -r .status)
  [ "$status" = "dlq" ] && break
  sleep 2
done

if [ "$status" != "dlq" ]; then
  echo "   TIMED OUT — event is still '${status}' after 180s" >&2
  curl -fsS "${API_URL}/v1/events/${event_id}" | jq '{status, attempt_count, attempts: [.attempts[] | {attempt_number, status_code}]}' >&2
  exit 1
fi

hr
echo "2. attempt history, with the gap between each attempt"
hr
curl -fsS "${API_URL}/v1/events/${event_id}" | jq -r '
  .attempts
  | to_entries
  | map(
      .value as $a
      | .key as $i
      | "   attempt \($a.attempt_number)  ->  HTTP \($a.status_code)   at \($a.attempted_at)"
    )
  | .[]'
echo ""
echo "   gaps between attempts (full jitter: the CEILING doubles, the actual"
echo "   draw is uniform in [0, ceiling), so gaps trend up but are not monotonic)"
# Postgres timestamps carry fractional seconds, which fromdateiso8601 rejects,
# so they are trimmed before parsing.
curl -fsS "${API_URL}/v1/events/${event_id}" | jq -r '
  [.attempts[].attempted_at | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601] as $t
  | [range(1; ($t|length))]
  | map("   attempt \(.) -> \(.+1):  \($t[.] - $t[.-1])s   (ceiling was \(pow(2; .))s)")
  | .[]'
echo ""
echo "   final status: $(curl -fsS "${API_URL}/v1/events/${event_id}" | jq -r .status)"
echo "   deliveries the sink actually received: $(curl -fsS "${SINK_CONTROL}/_control/stats" | jq -r ".per_event[\"${event_id}\"] // 0")"
echo "   duplicate dispatches (same event AND attempt): $(curl -fsS "${SINK_CONTROL}/_control/stats" | jq -c .duplicate_sends)"

hr
echo "3. sink flipped to 200; replaying from the DLQ"
hr
curl -fsS -X POST "${SINK_CONTROL}/_control/behavior" \
  -H 'Content-Type: application/json' -d '{"status":200}' >/dev/null

replay_code=$(curl -fsS -o /tmp/wr-replay.json -w '%{http_code}' -X POST "${API_URL}/v1/events/${event_id}/replay")
echo "   POST /v1/events/${event_id}/replay  ->  HTTP ${replay_code}"
echo "   attempt budget reset to $(jq -r .attempt_count /tmp/wr-replay.json)"

deadline=$(( $(date +%s) + 60 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  status=$(curl -fsS "${API_URL}/v1/events/${event_id}" | jq -r .status)
  [ "$status" = "delivered" ] && break
  sleep 1
done
echo "   final status: ${status}"
echo ""
echo "   full attempt history (the pre-replay failures are KEPT):"
curl -fsS "${API_URL}/v1/events/${event_id}" | jq -r '
  .attempts[] | "   attempt \(.attempt_number)  ->  HTTP \(.status_code)"'

hr
echo "4. verifying a delivered signature by hand"
hr
record=$(curl -fsS "${SINK_CONTROL}/_control/records" | jq -c "[.[] | select(.event_id==\"${event_id}\")] | last")
sig=$(echo "$record" | jq -r .signature)
body=$(echo "$record" | jq -r .body)
ts=$(echo "$sig" | sed -n 's/^t=\([0-9]*\).*/\1/p')
v1=$(echo "$sig" | sed -n 's/.*v1=\([0-9a-f]*\).*/\1/p')

echo "   header:        ${sig}"
echo "   signed payload: ${ts}.${body}"
echo ""
computed=$(printf '%s.%s' "$ts" "$body" | openssl dgst -sha256 -hmac "$secret" -r | awk '{print $1}')
echo "   openssl HMAC-SHA256: ${computed}"
echo "   header v1=:          ${v1}"
if [ "$computed" = "$v1" ]; then
  echo "   MATCH — the signature verifies independently of our Go code"
else
  echo "   MISMATCH" >&2
  exit 1
fi
rm -f /tmp/wr-replay.json
hr
echo "done."
