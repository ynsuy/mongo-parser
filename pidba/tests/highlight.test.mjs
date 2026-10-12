import assert from "node:assert/strict";
import test from "node:test";
import { dateColour, highlight, numberColour } from "../.pi/lib/highlight.ts";

test("colours numbers and dates in prose, not names, code or list markers", () => {
  const text = "3. `node-a:27017@dc1` had query.collscans at p95 10.82/s and 2,763 updates totaling 408 s at 2026-10-08 09:53 UTC, version 8.0.26-11 on rs0-1.";
  const want = "3. `node-a:27017@dc1` had query.collscans at p95 " + numberColour("10.82/s") + " and " + numberColour("2,763") +
    " updates totaling " + numberColour("408 s") + " at " + dateColour("2026-10-08 09:53 UTC") + ", version 8.0.26-11 on rs0-1.";
  assert.equal(highlight(text), want);
});

test("leaves code blocks exactly as drawn", () => {
  const chart = "```\n   13.5 ┤  ▂█\n      0 ┤▅▆██\n         08:00 UTC\n```";
  assert.equal(highlight(chart), chart);
});

test("keeps a unit only when it is a whole word", () => {
  assert.equal(highlight("10 seconds and 52-61 ms"), numberColour("10") + " seconds and " + numberColour("52") + "-" + numberColour("61 ms"));
});

test("colours a compound duration as one figure", () => {
  assert.equal(highlight("408 s (6m48s), 1h2m past"), numberColour("408 s") + " (" + numberColour("6m48s") + "), " + numberColour("1h2m") + " past");
});
