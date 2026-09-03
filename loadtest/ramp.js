// Ramp: 0 to 5000 events/sec over 10 minutes.
//
// The point is to find where it breaks, so the thresholds here are DELIBERATELY
// not the shared ones. A ramp that must stay under 50ms p95 would fail the
// moment it found the knee, which is the one thing this script exists to do.
//
// Instead it records the shape and lets the results table say where the knee
// was. `abortOnFail` on the error-rate threshold stops the run once the system
// is comprehensively broken, so the tail of the run is not thirty thousand
// identical connection errors.
import {
  registerEndpoint, postEvent, buildSummary,
} from "./lib/common.js";

export const options = {
  scenarios: {
    ramp: {
      executor: "ramping-arrival-rate",
      startRate: 50,
      timeUnit: "1s",
      preAllocatedVUs: 100,
      maxVUs: 2000,
      stages: [
        { target: 250, duration: "1m" },
        { target: 500, duration: "1m" },
        { target: 1000, duration: "2m" },
        { target: 2000, duration: "2m" },
        { target: 3500, duration: "2m" },
        { target: 5000, duration: "2m" },
      ],
    },
  },
  thresholds: {
    // Abort once more than 20% of requests are failing: past that the system
    // is not degrading, it is down, and further data is noise.
    http_req_failed: [{ threshold: "rate<0.20", abortOnFail: true, delayAbortEval: "30s" }],
  },
};

export function setup() {
  return { endpointId: registerEndpoint(__ENV.SINK_URL || "http://sink:9090/hook", "k6 ramp") };
}

export default function (data) {
  postEvent(data.endpointId, __VU, __ITER);
}

export function handleSummary(data) {
  const tag = __ENV.RESULT_TAG || "ramp";
  const out = {};
  out.stdout = buildSummary(data, "ramp 0 to 5000/s");
  out["loadtest/results/" + tag + ".json"] = JSON.stringify(data, null, 2);
  return out;
}
