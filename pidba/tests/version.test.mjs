import assert from "node:assert/strict";
import test from "node:test";
import { compactParserVersion, parseParserVersion } from "../.pi/lib/version.ts";

test("parses a stamped parser build", () => {
  const build = parseParserVersion(
    "mongo-log-parser 0.5.0+129a935ce18e, built 2026-10-06T06:24:51Z, go1.26.3 darwin/arm64",
  );
  assert.deepEqual(build, {
    raw: "mongo-log-parser 0.5.0+129a935ce18e, built 2026-10-06T06:24:51Z, go1.26.3 darwin/arm64",
    version: "0.5.0+129a935ce18e",
    builtAt: "2026-10-06T06:24:51Z",
    goVersion: "go1.26.3",
    platform: "darwin/arm64",
  });
  assert.equal(compactParserVersion(build), "0.5.0");
});

// A build from a git tree stamps the revision on the end. That field appeared the day the parser
// got a repository, and a reader anchored to the end of the line called every build unidentifiable.
test("reads a build that names its revision", () => {
  const build = parseParserVersion(
    "mongo-log-parser v0.5.0, built 2026-10-06T07:39:17Z, go1.26.3 darwin/arm64, revision 1a081b3d961b",
  );
  assert.equal(build.version, "v0.5.0");
  assert.equal(build.builtAt, "2026-10-06T07:39:17Z");
  assert.equal(build.revision, "1a081b3d961b");
  assert.equal(compactParserVersion(build), "v0.5.0");
});

// Whatever a future build adds, the version it opens with is still readable.
test("ignores a field it does not know", () => {
  const build = parseParserVersion("mongo-log-parser v0.6.0, built 2026-11-01T00:00:00Z, dirty");
  assert.equal(build.version, "v0.6.0");
  assert.equal(build.builtAt, "2026-11-01T00:00:00Z");
});

test("keeps unknown parser output inspectable", () => {
  const build = parseParserVersion("legacy parser");
  assert.equal(build.raw, "legacy parser");
  assert.equal(build.version, "unknown");
  assert.equal(compactParserVersion(build), "unknown");
});
