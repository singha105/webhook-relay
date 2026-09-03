// Soak: 500 events/sec for 30 minutes.
//
// Looks for what a five-minute run cannot see: memory that climbs and never
// comes back, a connection pool that leaks, latency that drifts upward as a
// table grows or an index bloats, goroutines that accumulate.
//
// The threshold is on DRIFT, not on absolute latency. A system that starts at
// 5ms and ends at 45ms passes a "p95 < 50ms" check while being obviously
// broken, so this run additionally compares the last five minutes against the
// first five — see loadtest/analyze-soak.sh, which reads the raw JSON.
import {
  thresholds, registerEndpoint, postEvent, buildSummary,
} from "./lib/common.js";

export const options = {
  scenarios: {
    soak: {
      executor: "constant-arrival-rate",
      rate: 500,
      timeUnit: "1s",
      duration: __ENV.SOAK_DURATION || "30m",
      preAllocatedVUs: 100,
      maxVUs: 400,
    },
  },
  thresholds: thresholds,
};

export function setup() {
  return { endpointId: registerEndpoint(__ENV.SINK_URL || "http://sink:9090/hook", "k6 soak") };
}

export default function (data) {
  postEvent(data.endpointId, __VU, __ITER);
}

export function handleSummary(data) {
  const tag = __ENV.RESULT_TAG || "soak";
  const out = {};
  out.stdout = buildSummary(data, "soak 500/s");
  out["loadtest/results/" + tag + ".json"] = JSON.stringify(data, null, 2);
  return out;
}
