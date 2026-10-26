import test from "node:test";
import assert from "node:assert/strict";
import { selectedUpstreamFromChunk, settledHook } from "./switchboard.ts";

test("OpenRouter metadata records the selected upstream, not the model author", () => {
  const chunk = { id: "gen-1", model: "deepseek/deepseek-v4", openrouter_metadata: {
    endpoints: { available: [
      { provider: "First Provider", selected: false },
      { provider: "DeepInfra", selected: true },
    ] },
  } };
  assert.deepEqual(selectedUpstreamFromChunk(chunk), { responseId: "gen-1", upstream: "deepinfra" });
  assert.equal(selectedUpstreamFromChunk({ id: "gen-2", model: "deepseek/deepseek-v4" }), undefined);
});

test("settledHook should report a cancelled Stop when Pi 1.1+ marks agent_settled aborted", () => {
  assert.deepEqual(settledHook({ aborted: true }, { stopReason: "stop" }), { hook: "Stop", aborted: true });
});

test("settledHook should report a cancelled Stop when older Pi ends the last message as aborted", () => {
  assert.deepEqual(settledHook({}, { stopReason: "aborted" }), { hook: "Stop", aborted: true });
  assert.deepEqual(settledHook(undefined, { stopReason: "aborted" }), { hook: "Stop", aborted: true });
});

test("settledHook should keep a StopFailure when an error stop was then cancelled", () => {
  assert.deepEqual(settledHook({ aborted: true }, { stopReason: "error" }), { hook: "StopFailure", aborted: false });
});

test("settledHook should report a plain Stop when the run finished or the payload is malformed", () => {
  assert.deepEqual(settledHook({ aborted: false }, { stopReason: "stop" }), { hook: "Stop", aborted: false });
  assert.deepEqual(settledHook({ aborted: "yes" }, undefined), { hook: "Stop", aborted: false });
  assert.deepEqual(settledHook(null, { stopReason: "toolUse" }), { hook: "Stop", aborted: false });
});
