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
    const selected = selectPath('"./mongo bundle" why are queries slow?', root);
    assert.equal(selected.path, realpathSync(join(root, "mongo bundle")));
    assert.equal(selected.text, "[selected local bundle] why are queries slow?");
    assert.equal(selectPath(`The issue is in ${root}/other`, root).path, realpathSync(join(root, "other")));
    assert.equal(selectPath("why is it slow?", root).path, undefined);
    assert.equal(selectPath("./missing why is it slow?", root).path, undefined);
    assert.throws(() => selectPath('"./mongo bundle" compare ./other', root), /one bundle directory/);
    assert.equal(selectPath("serverStatus/wiredTiger/cache/bytes", root).path, undefined);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
