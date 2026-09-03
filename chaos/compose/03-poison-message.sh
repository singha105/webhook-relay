#!/usr/bin/env bash
#
# Experiment 3 on the Compose stack: can one bad message wedge a worker?
#
# PREDICTION
# No. The worker never PARSES the payload — the API stores it as JSONB and the
# worker forwards it to the receiver as opaque bytes — so there is no
# user-controlled code path in the delivery loop that can panic on content. The
# API validates JSON at ingest, so malformed bodies get a 400 and never reach a
# worker.
set -euo pipefail
API="${API_URL:-http://localhost:8090}"
SINK_TARGET="${SINK_TARGET:-http://sink:9090/hook}"
command -v jq >/dev/null || { echo "need jq" >&2; exit 1; }

EP=$(curl -fsS -X POST "$API/v1/endpoints" -H 'Content-Type: application/json' \
      -d "{\"url\":\"${SINK_TARGET}\",\"description\":\"poison\",\"rate_limit_per_sec\":1000}" | jq -r .id)
echo "  endpoint $EP"

post() {
  local body="$1" label="$2"
  local code
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$API/v1/events" \
    -H 'Content-Type: application/json' \
    -d "{\"endpoint_id\":\"${EP}\",\"event_type\":\"poison\",\"payload\":${body}}" || echo "000")
  printf '  %-28s -> HTTP %s\n' "$label" "$code"
}

echo "  probing payloads the delivery path might choke on:"
post '{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":{"i":1}}}}}}}}}' "deeply nested object"
post '{"n":1e308}'                                            "float at max double"
post '{"n":1e-320}'                                           "denormal float"
post '{"a":[[[[[[[[[[1]]]]]]]]]]}'                            "deeply nested arrays"
post '{"empty":{}}'                                           "empty object"
post '[]'                                                     "top-level array"
post '"scalar"'                                               "top-level scalar"
post 'null'                                                   "top-level null"
post '{"dup":1,"dup":2}'                                      "duplicate keys"
post '{"big":"'"$(head -c 3000 /dev/zero | tr '\0' 'x')"'"}'  "3KB string"
post '{bad json'                                              "malformed JSON"

echo ""
echo "  waiting for the pipeline to settle..."
sleep 20

PG=$(docker compose -f deploy/compose/docker-compose.yml --env-file .env ps -q postgres)
echo "  event states:"
docker exec "$PG" psql -U webhook_relay -d webhook_relay -tAc \
  "SELECT '    ' || status || ': ' || count(*) FROM events WHERE endpoint_id='${EP}' GROUP BY status ORDER BY 1;"
echo "  worker restarts since start: $(docker inspect webhook-relay-worker-1 --format '{{.RestartCount}}' 2>/dev/null)"
echo "  worker state: $(docker inspect webhook-relay-worker-1 --format '{{.State.Status}}' 2>/dev/null)"
echo ""
echo "  A wedged worker would show as a nonzero restart count, or events stuck"
echo "  in a non-terminal state with the worker still running."
