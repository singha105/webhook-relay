// Baseline: a constant 200 events/sec for 5 minutes.
//
// This is the number every other run is compared against, so it uses an
// arrival-rate executor rather than a fixed VU count. The distinction matters:
// with `vus: N` and no sleep, k6 sends as fast as the system responds, so a
// slower system produces LESS load and latency looks artificially stable. A
// constant arrival rate holds the offered load fixed and lets the queue build,
// which is what actually happens when a real producer does not slow down
// because you are struggling.
import {
  thresholds, registerEndpoint, postEvent, buildSummary,
} from "./lib/common.js";

export const options = {
  scenarios: {
    baseline: {
      executor: "constant-arrival-rate",
      rate: 200,
      timeUnit: "1s",
      duration: "5m",
      // Pre-allocation matters: k6 warns and degrades if it has to grow the
      // VU pool mid-run, which shows up as a latency artefact that is really
      // k6's own scheduling.
      preAllocatedVUs: 50,
      maxVUs: 200,
    },
  },
  thresholds: thresholds,
};

export function setup() {
  return { endpointId: registerEndpoint(__ENV.SINK_URL || "http://sink:9090/hook", "k6 baseline") };
}

export default function (data) {
  postEvent(data.endpointId, __VU, __ITER);
}

export function handleSummary(data) {
  const tag = __ENV.RESULT_TAG || "baseline";
  const out = {};
  out.stdout = buildSummary(data, "baseline 200/s");
  out["loadtest/results/" + tag + ".json"] = JSON.stringify(data, null, 2);
  return out;
}
