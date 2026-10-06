import assert from "node:assert/strict";
import test from "node:test";
import { compactParserVersion, parseParserVersion } from "../.pi/lib/version.ts";

test("parses a stamped parser build", () => {
  const build = parseParserVersion(
    "mongo-log-parser 0.5.0+129a935ce18e, built 2026-09-21T14:35:06Z, go1.26.3 darwin/arm64",
  );
  assert.deepEqual(build, {
    raw: "mongo-log-parser 0.5.0+129a935ce18e, built 2026-09-21T14:35:06Z, go1.26.3 darwin/arm64",
    version: "0.5.0+129a935ce18e",
    builtAt: "2026-09-21T14:35:06Z",
    goVersion: "go1.26.3",
    platform: "darwin/arm64",
  });
  assert.equal(compactParserVersion(build), "0.5.0");
});

test("keeps unknown parser output inspectable", () => {
  const build = parseParserVersion("legacy parser");
  assert.equal(build.raw, "legacy parser");
  assert.equal(build.version, "unknown");
  assert.equal(compactParserVersion(build), "unknown");
});
