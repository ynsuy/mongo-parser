import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import test from "node:test";
import localModel from "../.pi/extensions/local-model.ts";

function setup() {
  let config, handler;
  localModel({
    registerProvider(_id, value) { config = value; },
    on(name, value) { if (name === "before_provider_request") handler = value; },
  });
  return { config, handler };
}

test("local request keeps tool definitions and asks for the shortest reasoning", () => {
  const { config, handler } = setup();
  const payload = { model: "pidba-gpt-oss:20b", tools: [{ type: "function" }] };
  const result = handler({ payload }, { model: {
    provider: "pidba-local", id: payload.model, baseUrl: config.baseUrl,
  } });
  assert.deepEqual(result.tools, payload.tools);
  // gpt-oss drops a level it does not know, so the value sent has to be one it accepts.
  assert.equal(result.reasoning_effort, "low");
  assert.equal(config.models[0].contextWindow, 32768);
});

for (const change of [
  { provider: "cloud" },
  { baseUrl: "https://example.com/v1" },
  { id: "gpt-oss:cloud" },
  { payloadModel: "gpt-oss:cloud" },
]) {
  test(`request blocked before transport: ${JSON.stringify(change)}`, () => {
    const child = spawnSync(process.execPath, ["--input-type=module", "-e", `
      import localModel from ${JSON.stringify(new URL("../.pi/extensions/local-model.ts", import.meta.url).href)};
      let handler;
      localModel({ registerProvider() {}, on(_name, value) { handler = value; } });
      const change = JSON.parse(process.argv[1]);
      handler({ payload: { model: change.payloadModel ?? "pidba-gpt-oss:20b" } }, { model: {
        provider: "pidba-local", id: "pidba-gpt-oss:20b", baseUrl: "http://127.0.0.1:11434/v1", ...change,
      } });
      console.log("transport would run");
    `, JSON.stringify(change)], { encoding: "utf8" });
    assert.equal(child.status, 1);
    assert.match(child.stderr, /blocked a non-local model request/);
    assert.doesNotMatch(child.stdout, /transport would run/);
  });
}
