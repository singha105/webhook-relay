#!/usr/bin/env bash
#
# Registers a demo endpoint and posts a few events against it, then prints what
# it created. Idempotent in the sense that re-running makes new rows; it does
# not try to be clever about existing ones.
set -euo pipefail

API_URL="${API_URL:-http://localhost:8080}"

command -v curl >/dev/null || { echo "seed: curl is required" >&2; exit 1; }
if ! command -v jq >/dev/null; then
  echo "seed: jq is required (brew install jq)" >&2
  exit 1
fi

if ! curl -fsS "${API_URL}/readyz" >/dev/null 2>&1; then
  echo "seed: the api at ${API_URL} is not ready — run 'make up' first" >&2
  exit 1
fi

echo "==> registering an endpoint"
endpoint=$(curl -fsS -X POST "${API_URL}/v1/endpoints" \
  -H 'Content-Type: application/json' \
  -d '{
        "url": "https://example.com/webhooks/orders",
        "description": "demo orders endpoint",
        "rate_limit_per_sec": 25
      }')

endpoint_id=$(echo "${endpoint}" | jq -r '.id')
secret=$(echo "${endpoint}" | jq -r '.signing_secret')

echo "    id:     ${endpoint_id}"
# Only ever shown here, and only the prefix: this is the one and only time the
# API returns it.
echo "    secret: ${secret:0:14}... (shown once, never returned again)"

echo
echo "==> posting three events"
for type in order.created order.paid order.shipped; do
  event=$(curl -fsS -X POST "${API_URL}/v1/events" \
    -H 'Content-Type: application/json' \
    -d "{
          \"endpoint_id\": \"${endpoint_id}\",
          \"event_type\":  \"${type}\",
          \"payload\":     {\"order_id\": \"ord_$RANDOM\", \"amount\": $((RANDOM % 500 + 10))}
        }")
  echo "    $(echo "${event}" | jq -r '.event_type + "  " + .id + "  " + .status')"
done

echo
echo "==> posting the same event twice with one Idempotency-Key"
key="seed-idem-$(date +%s)"
for i in 1 2; do
  code=$(curl -fsS -o /tmp/wr-seed-idem.json -w '%{http_code}' -X POST "${API_URL}/v1/events" \
    -H 'Content-Type: application/json' \
    -H "Idempotency-Key: ${key}" \
    -d "{
          \"endpoint_id\": \"${endpoint_id}\",
          \"event_type\":  \"order.refunded\",
          \"payload\":     {\"order_id\": \"ord_refund\", \"amount\": 42}
        }")
  echo "    request ${i}: HTTP ${code}  event $(jq -r '.id' /tmp/wr-seed-idem.json)"
done
rm -f /tmp/wr-seed-idem.json

echo
echo "==> done"
echo "    list endpoints:  curl -s ${API_URL}/v1/endpoints | jq"
echo "    inspect event:   curl -s ${API_URL}/v1/events/<id> | jq"
