import assert from "node:assert/strict";
import test from "node:test";
import { mkdtempSync, mkdirSync, rmSync, realpathSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { selectPath } from "../.pi/lib/data-path.ts";

test("select and mask a bundle while preserving the user's problem", () => {
  const root = mkdtempSync(join(tmpdir(), "pidba-"));
  try {
    mkdirSync(join(root, "mongo bundle"));
    mkdirSync(join(root, "other"));
    const selected = selectPath('"./mongo bundle" sorgular neden yavaş?', root);
    assert.equal(selected.path, realpathSync(join(root, "mongo bundle")));
    assert.equal(selected.text, "[selected local bundle] sorgular neden yavaş?");
    assert.equal(selectPath(`Sorun ${root}/other içinde`, root).path, realpathSync(join(root, "other")));
    assert.equal(selectPath("neden yavaş?", root).path, undefined);
    assert.equal(selectPath("./missing neden yavaş?", root).path, undefined);
    assert.throws(() => selectPath('"./mongo bundle" ./other karşılaştır', root), /tek veri klasörü/);
    assert.equal(selectPath("serverStatus/wiredTiger/cache/bytes", root).path, undefined);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
