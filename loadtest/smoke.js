// Smoke: 1 VU for 1 minute.
//
// Not a load test. It answers one question — "is the system behaving at all?"
// — before a longer run spends thirty minutes discovering that the endpoint
// was misconfigured. Run it first, always.
import { sleep } from "k6";
import {
  API, thresholds, registerEndpoint, postEvent, buildSummary,
} from "./lib/common.js";

export const options = {
  vus: 1,
  duration: "1m",
  thresholds: thresholds,
};

export function setup() {
  return { endpointId: registerEndpoint(__ENV.SINK_URL || "http://sink:9090/hook", "k6 smoke") };
}

export default function (data) {
  postEvent(data.endpointId, __VU, __ITER);
  // ~2 requests/sec. Slow on purpose: this is a correctness check.
  sleep(0.5);
}

export function handleSummary(data) {
  return {
    stdout: buildSummary(data, "smoke"),
    "loadtest/results/smoke.json": JSON.stringify(data, null, 2),
  };
}
