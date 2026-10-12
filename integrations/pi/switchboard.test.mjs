import test from "node:test";
import assert from "node:assert/strict";
import { selectedUpstreamFromChunk } from "./switchboard.ts";

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
