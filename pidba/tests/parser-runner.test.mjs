import assert from "node:assert/strict";
import test from "node:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { ParserRunner, parserFailure } from "../.pi/lib/parser-runner.ts";

test("parallel requests cannot overlap a parser cache rebuild", async () => {
  const root = mkdtempSync(join(tmpdir(), "pidba-runner-"));
  const script = `
    const fs = require("node:fs");
    const [lock, log, id] = process.argv.slice(1);
    const fd = fs.openSync(lock, "wx");
    fs.appendFileSync(log, "start " + id + "\\n");
    setTimeout(() => {
      fs.closeSync(fd); fs.unlinkSync(lock);
      fs.appendFileSync(log, "end " + id + "\\n");
      process.stdout.write(id);
    }, 40);
  `;
  try {
    const runner = new ParserRunner();
    const outputs = await Promise.all(["coverage", "report"].map(id =>
      runner.run(process.execPath, ["-e", script, join(root, "lock"), join(root, "log"), id])));
    assert.deepEqual(outputs, ["coverage", "report"]);
    assert.equal(readFileSync(join(root, "log"), "utf8"), "start coverage\nend coverage\nstart report\nend report\n");
  } finally { rmSync(root, { recursive: true, force: true }); }
});

test("the actual parser stderr reaches the caller and a failure does not stop later commands", async () => {
  const runner = new ParserRunner();
  const failure = runner.run(process.execPath, ["-e", 'process.stderr.write("reading the bundle: database is locked"); process.exit(2)']);
  const next = runner.run(process.execPath, ["-e", 'process.stdout.write("complete JSON")']);
  await assert.rejects(failure, /Parser failed \(2\): reading the bundle: database is locked/);
  assert.equal(await next, "complete JSON");
});

test("an aborted queued call returns no evidence and does not block the queue", async () => {
  const runner = new ParserRunner();
  const controller = new AbortController();
  const first = runner.run(process.execPath, ["-e", 'setTimeout(() => process.stdout.write("first"), 40)']);
  const aborted = runner.run(process.execPath, ["-e", 'process.stdout.write("should not run")'], controller.signal);
  controller.abort();
  const next = runner.run(process.execPath, ["-e", 'process.stdout.write("next")']);
  const results = await Promise.allSettled([first, aborted, next]);
  assert.equal(results[0].value, "first");
  assert.equal(results[1].status, "rejected");
  assert.equal(results[2].value, "next");
});

test("output overflow reports that partial evidence was not accepted", () => {
  assert.match(parserFailure({ code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER" }), /No partial evidence was returned/);
});

test("coverage errors remain visible in the collapsed terminal result", async () => {
  const { loadExtensions } = await import("../node_modules/@earendil-works/pi-coding-agent/dist/core/extensions/loader.js");
  const extension = new URL("../.pi/extensions/mongodb.ts", import.meta.url).pathname;
  const loaded = await loadExtensions([extension], new URL("..", import.meta.url).pathname);
  assert.deepEqual(loaded.errors, []);
  const coverage = loaded.extensions[0].tools.get("mongo_coverage").definition;
  const diagnostic = "Parser failed (2): reading the bundle: database is locked";
  const rendered = coverage.renderResult({ isError: true, content: [{ type: "text", text: diagnostic }] },
    { expanded: false, isPartial: false }, { fg: (_color, text) => text }).render(120).join("\n");
  assert.ok(rendered.includes(diagnostic), rendered);
});
