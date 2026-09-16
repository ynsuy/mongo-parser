import assert from "node:assert/strict";
import test from "node:test";
import { PromptMask } from "../.pi/lib/privacy.ts";

const aliases = [
  "source-004 -> bundle",
  "source-001 -> node-1/diagnostic.data",
  "ns-001 -> shop.orders",
  "node-011 -> mongo-a.prod.internal:27017",
];

test("names carrying a separator are masked", () => {
  const mask = new PromptMask();
  mask.load(aliases);
  assert.equal(mask.apply("why is shop.orders slow"), "why is ns-001 slow");
  assert.equal(mask.apply("check node-1/diagnostic.data"), "check source-001");
  assert.equal(mask.apply("mongo-a.prod.internal:27017 stalled"), "node-011 stalled");
});

test("the supplied directory's own name stays out of the sentence", () => {
  const mask = new PromptMask();
  mask.load(aliases);
  // "bundle" is what the parser calls the path it was given; replacing it rewrites the question.
  assert.equal(
    mask.apply("Investigate the bundle and report what you find"),
    "Investigate the bundle and report what you find",
  );
});

test("an unlisted host still gets an identifier", () => {
  const mask = new PromptMask();
  mask.load(aliases);
  const masked = mask.apply("db-7.example.com is unreachable");
  assert.match(masked, /^node-x\d+ is unreachable$/);
});
