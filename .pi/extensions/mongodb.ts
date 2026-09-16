import type { ExtensionAPI, Theme } from "@earendil-works/pi-coding-agent";
import { Text } from "@earendil-works/pi-tui";
import { Type } from "typebox";
import { execFile } from "node:child_process";
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { selectPath, directory } from "../lib/data-path.js";
import { PromptMask } from "../lib/privacy.js";

const runFile = promisify(execFile);
const projectRoot = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
// The parser is a standalone tool; this extension only drives it and shows what it drew.
const parser = process.env.DBA_PARSER_BIN ?? resolve(projectRoot, "../dba-mongo-parser/bin/dba-mongo-parser");
let dataPath = process.env.DBA_DATA_PATH || undefined;
const startCwd = process.env.DBA_START_CWD ?? process.cwd();
let bundleVersion = dataPath ? 1 : 0;
const systemPrompt = readFileSync(resolve(projectRoot, ".pi/SYSTEM.md"), "utf8").trim();

// Which build is answering. The launcher rebuilds the parser on every start, so the binary is
// usually current — but it has two ways of not being (a pinned DBA_PARSER_BIN, or a missing source
// tree), and a session started from an arbitrary directory gives no clue which happened. The
// launcher passes what it did in PIDBA_PARSER_ORIGIN, and the parser knows what it is.
const pidbaVersion = process.env.PIDBA_VERSION ?? "unknown";
const piVersion = process.env.PIDBA_PI_VERSION ?? "unknown";
const parserOrigin = process.env.PIDBA_PARSER_ORIGIN ?? "";
let parserVersion = "unknown";

// rebuilt is false when the binary was reused or pinned rather than compiled at start-up, which is
// the only case where its age is worth putting in front of the user.
const rebuilt = parserOrigin.startsWith("built from");

async function readParserVersion(): Promise<string> {
  try {
    const { stdout } = await runFile(parser, ["--version"], { maxBuffer: 1 << 20 });
    return stdout.split("\n")[0].trim();
  } catch {
    return "unknown (the parser did not answer --version; it is older than this launcher)";
  }
}

function versionLines(): string[] {
  return [
    `pidba ${pidbaVersion}`,
    `  ${parserVersion}`,
    `  ${parserOrigin || "origin unknown"}`,
    `  pi ${piVersion}`,
  ];
}

type Report = { kind: string; data: Record<string, unknown>; render: string; footer: string };

function arg(name: string, value: unknown): string[] {
  return value === undefined || value === null || value === "" ? [] : [`--${name}`, String(value)];
}

function width(): number {
  return Math.max(80, Math.min(200, (process.stdout.columns ?? 120) - 4));
}

// Every call runs the parser with --obscure, so hostnames, paths and namespaces never reach the
// model; the ids it sees are stable across calls and can be handed straight back as arguments.
// The launcher also sets DBA_OBSCURE, which the parser obeys over any flag: if this line were ever
// edited away, the parser would still refuse to hand back a real name on the agent's behalf.
async function run(command: string, args: string[], signal?: AbortSignal): Promise<Report> {
  if (!dataPath) throw new Error("No bundle selected. Ask the user to include an existing directory path in their message or use /path.");
  try {
    const { stdout } = await runFile(parser, [command, dataPath, "--json", "--obscure", ...arg("width", width()), ...args], {
      signal,
      maxBuffer: 128 * 1024 * 1024,
    });
    return JSON.parse(stdout) as Report;
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code ?? "unknown";
    throw new Error(`parser failed (${code}); check the selected directory and parser installation locally.`);
  }
}

// The parser already drew the tables and charts; the model relays that text instead of inventing
// a rendering of its own, and reads the structured data beside it when it needs numbers.
function result(report: Report) {
  const text = report.footer ? `${report.render}\n\n${report.footer}` : report.render;
  return { content: [{ type: "text" as const, text }], details: { data: report } };
}

type ToolResult = { isError?: boolean; details?: { data?: Report } };

function renderer(title: string, summary: (report: Report) => string) {
  return {
    renderCall(_args: object, theme: Theme) {
      return new Text(`${theme.fg("accent", "◆")} ${theme.fg("toolTitle", title)}`, 0, 0);
    },
    renderResult(toolResult: ToolResult, options: { isPartial: boolean }, theme: Theme) {
      if (options.isPartial) return new Text(theme.fg("muted", "  parsing…"), 0, 0);
      if (toolResult.isError || !toolResult.details?.data) return new Text(theme.fg("error", "  ✗ parser failed"), 0, 0);
      const report = toolResult.details.data;
      return new Text(`${theme.fg("success", `  ✓ ${summary(report)}`)}\n${report.render}`, 0, 0);
    },
  };
}

const windowFlags = {
  from: Type.Optional(Type.String({ description: "RFC3339 start of the window" })),
  to: Type.Optional(Type.String({ description: "RFC3339 end of the window" })),
};

const mask = new PromptMask();

export default function mongodb(pi: ExtensionAPI) {
  // What the user types is masked with the same identifiers the parser prints, so a name mentioned
  // in the conversation and the same name in a report are recognisably one thing to the model.
  pi.on("input", async (event, ctx) => {
    let text = event.text;
    try {
      const selected = selectPath(text, startCwd);
      text = selected.text;
      if (selected.path && selected.path !== dataPath) {
        dataPath = selected.path;
        bundleVersion++;
        mask.load([]);
        ctx.ui.setStatus("path", `path: ${dataPath}`);
      }
    } catch (error) {
      ctx.ui.notify((error as Error).message, "error");
      return { action: "handled" };
    }
    if (dataPath) text = text.split(dataPath).join("[selected local bundle]");
    try {
      const report = await run("aliases", []);
      mask.load((report.data as unknown as string[]) ?? []);
    } catch {
      // An unreadable path is the tools' problem to report; the prompt still gets masked.
    }
    return { action: "transform", text: mask.apply(text), images: event.images };
  });

  pi.registerCommand("path", {
    description: "Select or show the local bundle directory: /path /data/mongodb",
    async handler(args, ctx) {
      if (!args.trim()) {
        ctx.ui.notify(dataPath ?? "Henüz klasör seçilmedi. Problemi klasör yoluyla birlikte yaz.", "info");
        return;
      }
      try {
        const value = args.trim().replace(/^(["'`])(.*)\1$/, "$2");
        const next = directory(value, startCwd);
        if (next !== dataPath) { dataPath = next; bundleVersion++; mask.load([]); }
        ctx.ui.setStatus("path", `path: ${dataPath}`);
        ctx.ui.notify("Veri klasörü seçildi. Şimdi problemi anlatabilirsin.", "info");
      } catch {
        ctx.ui.notify("Klasör bulunamadı veya okunamıyor; yolu kontrol et.", "error");
      }
    },
  });

  pi.registerCommand("aliases", {
    description: "Look up what a local identifier stands for: /aliases, or /aliases ns-004 to ask about one",
    async handler(args, ctx) {
      // An answer names ns-004; this is where that becomes a collection again. Terms work in both
      // directions, so a name you already have finds the identifier the model was given for it.
      const terms = args.trim().split(/\s+/).filter(Boolean);
      const report = await run("aliases", terms);
      const lines = (report.data as unknown as string[]) ?? [];
      await ctx.ui.select(terms.length ? `Local identifiers · ${terms.join(" ")}` : "Local identifiers", lines);
    },
  });

  pi.registerCommand("version", {
    description: "Which build of the launcher, the parser and pi this session is running",
    async handler(_args, ctx) {
      await ctx.ui.select("Versions", versionLines());
    },
  });

  pi.on("session_start", async (_event, ctx) => {
    ctx.ui.setTitle("pidba · MongoDB");
    ctx.ui.setStatus("path", `path: ${dataPath ?? "problemi klasör yoluyla birlikte yaz"}`);
    parserVersion = await readParserVersion();
    // The short form goes in the status bar, where it is visible for the whole session without
    // being asked for: an answer is only as current as the binary that produced it.
    ctx.ui.setStatus("version", `pidba ${pidbaVersion} · ${parserVersion.replace(/^dba-mongo-parser /, "parser ").split(",")[0]}`);
    if (!rebuilt) {
      ctx.ui.notify(`Parser bu oturumda yeniden derlenmedi (${parserOrigin || "sebep bilinmiyor"}). Eski bir sürüm çalışıyor olabilir — /version`, "warning");
    }
    if (parserVersion.includes("dev build")) {
      ctx.ui.notify("Parser sürümsüz derlenmiş (dev build); hangi kodu çalıştırdığı tespit edilemez.", "warning");
    }
  });

  // Pi's own prompt introduces a coding assistant with file and shell tools. This agent has
  // neither — every answer comes from dba-mongo-parser through the mongo_* tools — so the prompt is
  // replaced rather than appended to, and SYSTEM.md is the whole of it.
  pi.on("before_agent_start", async () => ({
    systemPrompt: `${systemPrompt}

You are running ${parserVersion}, driven by pidba ${pidbaVersion}. If the user asks which version of
anything they are using, or why an answer differs from one they remember, say this and point them at
/version; do not guess at a release number or a changelog.

${dataPath
  ? `Local bundle revision ${bundleVersion} is selected. All tools read it without a path argument. If the revision changed, discard earlier bundle findings and run a fresh report; never mix evidence from different bundles.`
  : "No local bundle is selected. Ask the user for the existing local directory containing the logs/FTDC, together with the problem. Do not call tools until a bundle is selected. Use /path if automatic selection did not recognize it."}`,
  }));

  pi.registerTool({
    name: "mongo_report",
    label: "MongoDB path report",
    description: "Read the whole path in one pass and report what stands out: what was running (server version, role, storage engine, cache size, restarts and unclean shutdowns), the findings the screening rules produced, a chart of how much was logged over time split by severity, a chart for the most notable metric of each FTDC stream, the slow query shapes that cost the most time and where that time went, replication and sharding events per node, the catalogue changes (index builds, drops, renames), the conditions the server logged without raising a severity, and the log groups worth reviewing. This is the right first call for an unexplained problem; the other tools drill into what it surfaces. Use only to keep one section when you already know which one you want.",
    parameters: Type.Object({
      only: Type.Optional(Type.Array(Type.String(), { maxItems: 9, description: "Sections to print: deployment, findings, activity, metrics, slow, replication, ddl, signals, logs. Every section by default." })),
      top: Type.Optional(Type.Integer({ minimum: 1, maximum: 20, description: "How many metric charts to draw" })),
      height: Type.Optional(Type.Integer({ minimum: 2, maximum: 12, description: "Chart rows per metric" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal) {
      return result(await run("report", [
        ...arg("only", (p.only ?? []).join(",")), ...arg("top", p.top), ...arg("height", p.height),
        ...arg("from", p.from), ...arg("to", p.to),
      ], signal));
    },
    ...renderer("mongo path report", (report) => {
      const findings = (report.data.findings as unknown[])?.length ?? 0;
      const shapes = (report.data.slow_queries as unknown[])?.length ?? 0;
      return `${findings} findings · ${shapes} query shapes`;
    }),
  });

  pi.registerTool({
    name: "mongo_inventory",
    label: "MongoDB path inventory",
    description: "List what the bundle holds: FTDC streams and how many log files there are. Call this first to learn the stream identifiers other tools take.",
    parameters: Type.Object({}),
    async execute(_id, _params, signal) {
      return result(await run("inventory", [], signal));
    },
    ...renderer("mongo inventory", (report) => `${(report.data.ftdc_streams as unknown[])?.length ?? 0} FTDC streams · ${report.data.log_files ?? 0} log files`),
  });

  pi.registerTool({
    name: "mongo_metrics",
    label: "MongoDB FTDC metrics",
    description: "Chart FTDC metrics over a window. Name metrics to see exactly those, or omit them and the parser ranks the performance-relevant ones (tickets, queues, cache, latency, opcounters, cpu, disk) by importance and by what moved. Name several sources to compare nodes: each stream is read over the same window and gets its own colour, and the rows of one metric stay together. Set overlay to put every node's line on one chart when the question is where two nodes stopped agreeing.",
    parameters: Type.Object({
      sources: Type.Optional(Type.Array(Type.String(), { maxItems: 12, description: "Stream identifiers from mongo_inventory; several to compare nodes, or [\"all\"] for every stream. The only stream by default." })),
      metrics: Type.Optional(Type.Array(Type.String(), { maxItems: 20, description: "Metric paths, e.g. serverStatus/globalLock/currentQueue/total. * and ? match, so a subtree can be asked for whole: flowControl/*, wiredTiger/cache/*" })),
      overlay: Type.Optional(Type.Boolean({ description: "Draw every stream of a metric on one chart, a colour per stream, instead of a chart each" })),
      top: Type.Optional(Type.Integer({ minimum: 1, maximum: 40, description: "How many metrics to rank when none are named" })),
      height: Type.Optional(Type.Integer({ minimum: 2, maximum: 20, description: "Chart rows per metric" })),
      bin: Type.Optional(Type.Integer({ minimum: 1, description: "Bin size in seconds; chosen from the window by default" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal) {
      const sources = (p.sources ?? []).flatMap((source) => ["--source", source]);
      const metrics = (p.metrics ?? []).flatMap((metric) => ["--metric", metric]);
      return result(await run("metrics", [
        ...sources, ...metrics, ...arg("top", p.top), ...arg("height", p.height),
        ...arg("bin", p.bin), ...arg("from", p.from), ...arg("to", p.to),
        ...(p.overlay ? ["--overlay"] : []),
      ], signal));
    },
    ...renderer("mongo FTDC metrics", (report) => `${(report.data as unknown as unknown[])?.length ?? 0} metrics charted`),
  });

  pi.registerTool({
    name: "mongo_logs",
    label: "MongoDB log groups",
    description: "Group every structured log line in the bundle, rotations and gzip included, and rank the groups by how much they matter: severity first, then failure signals, then event kind. A chart of log volume over time is drawn above the table, split into errors, warnings and everything else, with the severity and category totals under it. Narrow with a category, a minimum severity, a node or a substring, and pass a group id to read the lines themselves with their attributes.",
    parameters: Type.Object({
      top: Type.Optional(Type.Integer({ minimum: 1, maximum: 100, description: "How many groups to show" })),
      category: Type.Optional(Type.String({ description: "slow_query, election, initial_sync, recovery, replication, sharding, ddl, storage, network, authentication, query or other" })),
      severity: Type.Optional(Type.String({ description: "Keep this severity and worse: F, E, W or I" })),
      source: Type.Optional(Type.String({ description: "Source identifier from an earlier result: only the groups seen on that node" })),
      contains: Type.Optional(Type.String({ description: "Only the groups whose message contains this text" })),
      group: Type.Optional(Type.String({ description: "Group id from an earlier result: shows the lines themselves instead of the summary" })),
      records: Type.Optional(Type.Integer({ minimum: 1, maximum: 50, description: "How many lines to show for a group" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal) {
      return result(await run("logs", [
        ...arg("top", p.top), ...arg("category", p.category), ...arg("severity", p.severity),
        ...arg("source", p.source), ...arg("contains", p.contains), ...arg("group", p.group),
        ...arg("records", p.records), ...arg("from", p.from), ...arg("to", p.to),
      ], signal));
    },
    ...renderer("mongo log groups", (report) => `${report.data.total_groups ?? 0} groups from ${(report.data.quality as Record<string, unknown>)?.records_parsed ?? 0} records`),
  });

  pi.registerTool({
    name: "mongo_slow",
    label: "MongoDB slow queries",
    description: "Rank slow query shapes by the total time they cost the server, with count, average, p95 and maximum duration, documents and keys examined, inefficiency (documents read per document returned), bytes read, and what each shape mostly waited for. Under the table, a breakdown of where the time went across the whole workload: flow control, write concern, lock acquire, ticket queue, storage read, remote shards, cpu, and what the log did not account for. A slow operation that waited and one that did work print the same duration and are different problems, so read the breakdown before blaming a query. Naming a filter — ns, plan, query_hash or waited_on — also adds a drill-down for the costliest shape it kept: a duration chart (min, average and worst per bin), an executions chart, the full distribution of every measure with min, average, p50, p95, p99, max and total, and the slowest real logged executions with the command document each one ran. Under masking those commands keep their field names and have every value replaced by its type, which is what an index decision depends on and is safe to reason about. That is the view to use once the ranking has told you which shape to look at, because a single average cannot tell an operation that is reliably slow from one that is usually fine with a bad tail. Query hashes are the original logged values, never aliases. Rows without a hash are namespace/operation/plan groups, not known query shapes.",
    parameters: Type.Object({
      top: Type.Optional(Type.Integer({ minimum: 1, maximum: 100, description: "How many shapes to show" })),
      plan: Type.Optional(Type.String({ description: "COLLSCAN, IXSCAN, IDHACK, COUNT or EOF" })),
      ns: Type.Optional(Type.String({ description: "Namespace identifier from an earlier result" })),
      op: Type.Optional(Type.String({ description: "Only these operations: a command name (find, aggregate, insert, update, delete, getMore, findAndModify), a comma-separated list of them, or a group: writes, reads, ddl" })),
      query_hash: Type.Optional(Type.String({ description: "Exact queryHash or planCacheShapeHash from the log" })),
      waited_on: Type.Optional(Type.String({ description: "Keep only the shapes whose biggest wait is this: flow control, write concern, lock acquire, ticket queue, storage read or remote shards" })),
      examples: Type.Optional(Type.Integer({ minimum: 0, maximum: 20, description: "How many real logged executions of the shape to show, slowest first, with the command each one ran. Defaults to 3 once a filter is given; 0 leaves them out." })),
      no_detail: Type.Optional(Type.Boolean({ description: "Leave out the drill-down a filter would otherwise add, keeping only the ranked table" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal) {
      return result(await run("slow", [
        ...arg("top", p.top), ...arg("plan", p.plan), ...arg("ns", p.ns), ...arg("op", p.op),
        ...arg("query-hash", p.query_hash), ...arg("waited-on", p.waited_on),
        ...arg("examples", p.examples), ...(p.no_detail ? ["--no-detail"] : []),
        ...arg("from", p.from), ...arg("to", p.to),
      ], signal));
    },
    ...renderer("mongo slow queries", (report) => {
      const waits = (report.data.waits as { name?: string; share?: number }[] | undefined) ?? [];
      const lead = waits.find((wait) => wait.name !== "unaccounted" && wait.name !== "cpu");
      const spent = lead ? ` · mostly ${lead.name} ${Math.round((lead.share ?? 0) * 100)}%` : "";
      return `${report.data.total_shapes ?? 0} shapes · ${report.data.collscan_shapes ?? 0} COLLSCAN${spent}`;
    }),
  });

  pi.registerTool({
    name: "mongo_deployment",
    label: "MongoDB deployment",
    description: "What was actually running: per process the role (mongod, mongos, shard server or config server), the version it is on now and the ones it was on before, when it last started, storage engine, WiredTiger cache size, replica set, whether authorization was on, how many times it started inside the window, and whether it ever came up after an unclean shutdown. Under the table, the version each process came up on at each restart in order — which is what separates a finished upgrade from one that stalled halfway or was rolled back, and a set of version strings cannot. All of it is written once at startup, so a bundle whose logs were rotated past the last restart reports that instead of a number. Call this before reading any other figure: a queue depth means one thing on a 4 GB cache and another on 200 GB, and a counter that reset at a restart is not a counter.",
    parameters: Type.Object({ ...windowFlags }),
    async execute(_id, p, signal) {
      return result(await run("deployment", [...arg("from", p.from), ...arg("to", p.to)], signal));
    },
    ...renderer("mongo deployment", (report) => {
      const summary = (report.data.summary as Record<string, unknown>) ?? {};
      return `${(report.data.processes as unknown[])?.length ?? 0} processes · ${((summary.versions as string[]) ?? []).join(", ") || "version not logged"}`;
    }),
  });

  pi.registerTool({
    name: "mongo_ddl",
    label: "MongoDB catalogue changes",
    description: "Index builds and catalogue changes with their timing: every build followed from the line that started it to the line that ended it, with how long it ran and whether it completed, failed, was aborted or never logged a completion — and beside them the drops, creates, renames, collMods, shardCollections and featureCompatibilityVersion changes. A build or a drop that overlapped a bad window is the cheapest explanation there is, and it appears in no metric. Filter by namespace identifier or by kind.",
    parameters: Type.Object({
      ns: Type.Optional(Type.String({ description: "Namespace identifier from an earlier result" })),
      kind: Type.Optional(Type.String({ description: "One kind only, e.g. \"index build\", \"drop collection\", \"drop index\", \"shard collection\"" })),
      top: Type.Optional(Type.Integer({ minimum: 1, maximum: 100, description: "How many rows of each table to show" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal) {
      return result(await run("ddl", [...arg("ns", p.ns), ...arg("kind", p.kind), ...arg("top", p.top), ...arg("from", p.from), ...arg("to", p.to)], signal));
    },
    ...renderer("mongo catalogue changes", (report) =>
      `${(report.data.index_builds as unknown[])?.length ?? 0} index builds · ${(report.data.changes as unknown[])?.length ?? 0} other changes`),
  });

  pi.registerTool({
    name: "mongo_health",
    label: "MongoDB logged conditions",
    description: "The conditions MongoDB logs at severity I and therefore never ranks by severity: authentication failures, write conflicts, lock timeouts, cursors reaped, operations killed, heartbeat failures, members reported unreachable, stale routing tables, jumbo chunks, connection churn, storage operations that took too long, clock skew. Each with a count, a rate per minute, when it started and stopped, how many nodes saw it, and what to look at next. These are the things a thousand of which is a problem and one of which is nothing, so read the rate rather than the count. A condition here is a pattern in the message text, not a verdict: confirm it with mongo_logs before acting on it.",
    parameters: Type.Object({
      top: Type.Optional(Type.Integer({ minimum: 1, maximum: 100, description: "How many conditions to show" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal) {
      return result(await run("health", [...arg("top", p.top), ...arg("from", p.from), ...arg("to", p.to)], signal));
    },
    ...renderer("mongo logged conditions", (report) => `${(report.data.signals as unknown[])?.length ?? 0} conditions`),
  });
}
