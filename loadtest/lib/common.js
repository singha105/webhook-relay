// Shared helpers for every k6 script.
//
// One file so the four scenarios differ ONLY in their load profile. If setup
// or thresholds drifted between scripts, a "before and after" comparison would
// be measuring the scripts as much as the service.
import http from "k6/http";
import { check } from "k6";
import { Counter, Trend } from "k6/metrics";

export const API = __ENV.API_URL || "http://localhost:8090";

// Custom metrics. The built-in http_req_duration covers every request the
// script makes, including setup; these isolate the ingest path, which is the
// only latency the SLO speaks about.
export const ingestDuration = new Trend("ingest_duration", true);
export const ingestErrors = new Counter("ingest_errors");
export const ingestAccepted = new Counter("ingest_accepted");

// Thresholds shared by every scenario.
//
// These are deliberately capable of failing the run. A load test whose
// thresholds sit above anything the system could plausibly produce is theatre:
// it reports green regardless and teaches nobody anything.
//
//   ingest p95 < 50ms   the ingest path does four things — decode, validate,
//                       one INSERT, respond. Against a local Postgres that is
//                       a few milliseconds of work, so 50ms is about 10x
//                       headroom: loose enough not to flake, tight enough that
//                       a regression in the hot path trips it.
//   error rate < 0.1%   ingest is the customer-facing boundary. A dropped POST
//                       is an event the customer must retry, so the bar is far
//                       stricter than for delivery, which retries by design.
export const thresholds = {
  ingest_duration: ["p(95)<50"],
  http_req_failed: ["rate<0.001"],
  checks: ["rate>0.999"],
};

const EVENT_TYPES = ["order.created", "order.paid", "order.shipped", "user.updated"];

// registerEndpoint creates a delivery target for the run.
//
// rateLimitPerSec is set to the maximum the API accepts. These scripts measure
// the ingest path and the delivery pipeline, not the rate limiter, and leaving
// the default of 10/s would mean every run measured the token bucket instead.
//
// 1000 is a hard ceiling: models.maxRateLimitPerSec rejects anything higher
// with a 400. That means a SINGLE endpoint cannot be driven past 1000
// deliveries/sec regardless of how many workers exist — a real constraint on
// this system, recorded in the results table rather than worked around.
// Ingest is unaffected: the rate limiter only gates delivery.
export function registerEndpoint(url, description, rateLimitPerSec) {
  const limit = rateLimitPerSec || 1000;
  const res = http.post(
    `${API}/v1/endpoints`,
    JSON.stringify({ url: url, description: description, rate_limit_per_sec: limit }),
    { headers: { "Content-Type": "application/json" } },
  );
  if (res.status !== 201) {
    throw new Error("could not register endpoint: " + res.status + " " + res.body);
  }
  return JSON.parse(res.body).id;
}

// postEvent sends one event and records the ingest metrics.
export function postEvent(endpointId, vu, iter) {
  const eventType = EVENT_TYPES[(vu + iter) % EVENT_TYPES.length];
  const payload = JSON.stringify({
    endpoint_id: endpointId,
    event_type: eventType,
    payload: {
      order_id: "ord_" + vu + "_" + iter,
      amount: (iter % 500) + 10,
      // A little bulk, so the payload is not trivially small. Real webhook
      // bodies are hundreds of bytes to a few KB; a 20-byte body would measure
      // the network stack rather than the service.
      note: "load test event, padded to a realistic size. ".repeat(3),
    },
  });

  const res = http.post(API + "/v1/events", payload, {
    headers: { "Content-Type": "application/json" },
    tags: { name: "ingest" },
  });

  ingestDuration.add(res.timings.duration);

  const ok = check(res, { "ingest returned 202": (r) => r.status === 202 });
  if (ok) {
    ingestAccepted.add(1);
  } else {
    ingestErrors.add(1);
  }
  return res;
}

// buildSummary renders a compact human-readable block. The raw JSON is written
// alongside it, so results/ carries both something to read and something a
// diff can compare.
export function buildSummary(data, name) {
  const m = data.metrics;
  const get = (metric, stat) =>
    m[metric] && m[metric].values && m[metric].values[stat] !== undefined
      ? m[metric].values[stat]
      : null;
  const fmt = (v, digits) => (v === null ? "n/a" : v.toFixed(digits === undefined ? 2 : digits));

  return [
    "",
    "  scenario:            " + name,
    "  duration:            " + fmt(data.state.testRunDurationMs / 1000, 1) + "s",
    "  events accepted:     " + (get("ingest_accepted", "count") || 0),
    "  ingest errors:       " + (get("ingest_errors", "count") || 0),
    "  throughput:          " + fmt(get("http_reqs", "rate")) + " req/s",
    "",
    "  ingest latency",
    "    p50:               " + fmt(get("ingest_duration", "med")) + " ms",
    "    p95:               " + fmt(get("ingest_duration", "p(95)")) + " ms",
    "    p99:               " + fmt(get("ingest_duration", "p(99)")) + " ms",
    "    max:               " + fmt(get("ingest_duration", "max")) + " ms",
    "",
    "  http_req_failed:     " + fmt((get("http_req_failed", "rate") || 0) * 100, 3) + " %",
    "",
  ].join("\n");
}
