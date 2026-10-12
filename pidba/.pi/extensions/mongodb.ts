import type { ExtensionAPI, ExtensionContext, Theme } from "@earendil-works/pi-coding-agent";
import { Text, truncateToWidth } from "@earendil-works/pi-tui";
import { Type } from "typebox";
import { execFile } from "node:child_process";
import { readFileSync } from "node:fs";
import { basename, dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { ParserRunner } from "../lib/parser-runner.js";
import { selectPath, directory } from "../lib/data-path.js";
import { pageBytes, pageHeader, parserResult, ResultPages, type ParserReport } from "../lib/tool-result.js";
import { compactParserVersion, parseParserVersion, type ParserBuild } from "../lib/version.js";

const runFile = promisify(execFile);
const projectRoot = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
// The parser is a standalone tool; this extension only drives it and shows what it drew.
const parser = process.env.MONGO_LOG_PARSER_BIN ?? resolve(projectRoot, "../mongo-log-parser/bin/mongo-log-parser");
let dataPath = process.env.DBA_DATA_PATH || undefined;
const startCwd = process.env.DBA_START_CWD ?? process.cwd();
let bundleVersion = dataPath ? 1 : 0;
const systemPrompt = readFileSync(resolve(projectRoot, ".pi/SYSTEM.md"), "utf8").trim();

// Which build is answering. The launcher rebuilds the parser on every start, so the binary is
// usually current — but it has two ways of not being (a pinned MONGO_LOG_PARSER_BIN, or a missing source
// tree), and a session started from an arbitrary directory gives no clue which happened. The
// launcher passes what it did in PIDBA_PARSER_ORIGIN, and the parser knows what it is.
const pidbaVersion = process.env.PIDBA_VERSION ?? "unknown";
const piVersion = process.env.PIDBA_PI_VERSION ?? "unknown";
const parserOrigin = process.env.PIDBA_PARSER_ORIGIN ?? "";
const parserSource = process.env.PIDBA_PARSER_SOURCE ?? "unknown";
const parserBinary = process.env.PIDBA_PARSER_BINARY ?? parser;
const provider = process.env.PIDBA_PROVIDER ?? "unknown";
const model = process.env.PIDBA_MODEL ?? "unknown";
let parserBuild: ParserBuild = parseParserVersion("");

// rebuilt is false when the binary was reused or pinned rather than compiled at start-up, which is
// the only case where its age is worth putting in front of the user.
const rebuilt = parserOrigin === "rebuilt from source";

async function readParserVersion(): Promise<ParserBuild> {
  try {
    const { stdout } = await runFile(parser, ["--version"], { maxBuffer: 1 << 20 });
    return parseParserVersion(stdout);
  } catch {
    return parseParserVersion("mongo-log-parser unknown");
  }
}

function versionLines(): string[] {
  const build = [parserBuild.builtAt, parserBuild.goVersion, parserBuild.platform].filter(Boolean).join(" · ");
  const lines = [
    `pidba ${pidbaVersion}`,
    `runtime  pi ${piVersion}`,
    `model    ${provider}/${model}`,
    `parser   ${parserBuild.version}`,
    `binary   ${parserBinary}`,
    `source   ${parserSource}`,
    `mode     ${parserOrigin || "unknown"}`,
  ];
  if (build) lines.splice(4, 0, `build    ${build}`);
  return lines;
}

function bundleStatus(): string {
  return dataPath ? `bundle ${basename(dataPath)}` : "bundle not selected";
}

function installCompactChrome(ctx: ExtensionContext): void {
  if (ctx.mode !== "tui") return;

  ctx.ui.setHeader((_tui, theme) => ({
    invalidate() {},
    render(width: number): string[] {
      const title = theme.fg("accent", `pidba ${pidbaVersion}`);
      const purpose = theme.fg("muted", " · local bundle analysis");
      return [truncateToWidth(title + purpose, width)];
    },
  }));

  ctx.ui.setFooter((_tui, theme) => ({
    invalidate() {},
    render(width: number): string[] {
      const usage = ctx.getContextUsage();
      const context = usage?.percent == null ? "" : ` · ctx ${Math.round(usage.percent)}%`;
      const line = `${bundleStatus()} · parser ${compactParserVersion(parserBuild)} · ${provider}/${model}${context}`;
      return [truncateToWidth(theme.fg("muted", line), width)];
    },
  }));
}

type Report = ParserReport;

function arg(name: string, value: unknown): string[] {
  return value === undefined || value === null || value === "" ? [] : [`--${name}`, String(value)];
}

function width(): number {
  return Math.max(80, Math.min(200, (process.stdout.columns ?? 120) - 4));
}

// The parser reads the bundle into a local store on the first call and answers from it afterwards,
// so the first question of a session is slower than the rest. Names are not masked: whatever the
// bundle holds reaches the model as it was written.
const parserRunner = new ParserRunner();

async function run(command: string, args: string[], signal?: AbortSignal): Promise<Report> {
  if (!dataPath) throw new Error("No bundle selected. Ask the user to include an existing directory path in their message or use /path.");
  const selectedPath = dataPath;
  const selectedRevision = bundleVersion;
  const stdout = await parserRunner.run(parser, [command, selectedPath, "--json", ...arg("width", width()), ...args], signal);
  try {
    return { ...JSON.parse(stdout), requested_args: args, bundle_revision: selectedRevision } as Report;
  } catch (error) {
    throw new Error(`Parser returned invalid JSON for ${command}; no evidence was accepted.`, { cause: error });
  }
}

// Every answer that needed more than one page, for mongo_page.
const pages = new ResultPages();

// Send the parser's structured evidence to the model, in pages sized to its context window when it is
// large, while the terminal keeps the parser's own presentation.
function result(ctx: ExtensionContext, report: Report) {
  return parserResult(report, typeof report.bundle_revision === "number" ? report.bundle_revision : bundleVersion, pageBytes(ctx.getContextUsage()?.contextWindow ?? ctx.model?.contextWindow), pages);
}

type ToolResult = { isError?: boolean; content?: { type: string; text?: string }[]; details?: { data?: Report } };

function renderer(title: string, summary: (report: Report) => string) {
  return {
    renderCall(_args: object, theme: Theme) {
      return new Text(`${theme.fg("accent", "◆")} ${theme.fg("toolTitle", title)}`, 0, 0);
    },
    renderResult(toolResult: ToolResult, options: { expanded: boolean; isPartial: boolean }, theme: Theme) {
      if (options.isPartial) return new Text(theme.fg("muted", "  … parsing"), 0, 0);
      if (toolResult.isError || !toolResult.details?.data) {
        const reason = toolResult.content?.filter(item => item.type === "text").map(item => item.text ?? "").join("\n").trim() || "Parser returned no evidence.";
        return new Text(theme.fg("error", `  ✗ ${reason}`), 0, 0);
      }
      const report = toolResult.details.data;
      const line = theme.fg("success", `  ✓ ${summary(report)}`);
      return new Text(options.expanded && report.render ? `${line}\n${report.render}` : line, 0, 0);
    },
  };
}

const windowFlags = {
  from: Type.Optional(Type.String({ description: "Start of the window, UTC: 2026-01-15T10:00:00Z, \"2026-01-15 10:00\", \"01-15 10:00\", or 10:00 when the bundle covers one day" })),
  to: Type.Optional(Type.String({ description: "End of the window, same forms as from" })),
  around: Type.Optional(Type.String({ description: "A moment to centre the window on, same forms as from" })),
  window: Type.Optional(Type.String({ description: "Width of the around window, e.g. 20m or 2h. 30m by default" })),
  last: Type.Optional(Type.String({ description: "A window this long ending at the last thing the bundle recorded, e.g. 2h" })),
};

// windowArgs turns those into flags; every command takes the same ones.
function windowArgs(p: { from?: string; to?: string; around?: string; window?: string; last?: string }): string[] {
  return [...arg("from", p.from), ...arg("to", p.to), ...arg("around", p.around),
          ...arg("window", p.window), ...arg("last", p.last)];
}

export default function mongodb(pi: ExtensionAPI) {
  pi.on("input", async (event, ctx) => {
    let text = event.text;
    try {
      const selected = selectPath(text, startCwd);
      text = selected.text;
      if (selected.path && selected.path !== dataPath) {
        dataPath = selected.path;
        bundleVersion++;
        ctx.ui.setStatus("bundle", bundleStatus());
      }
    } catch (error) {
      ctx.ui.notify((error as Error).message, "error");
      return { action: "handled" };
    }
    return { action: "transform", text, images: event.images };
  });

  pi.registerCommand("path", {
    description: "Select or show the local bundle directory: /path /data/mongodb",
    async handler(args, ctx) {
      if (!args.trim()) {
        ctx.ui.notify(dataPath ?? "No bundle selected. Include its directory in your message or use /path.", "info");
        return;
      }
      try {
        const value = args.trim().replace(/^(["'`])(.*)\1$/, "$2");
        const next = directory(value, startCwd);
        if (next !== dataPath) { dataPath = next; bundleVersion++; }
        ctx.ui.setStatus("bundle", bundleStatus());
        ctx.ui.notify(`Bundle selected: ${dataPath}`, "info");
      } catch {
        ctx.ui.notify("Directory not found or unreadable. Check the path and try again.", "error");
      }
    },
  });

  pi.registerCommand("version", {
    description: "Which build of the launcher, the parser and pi this session is running",
    async handler(_args, ctx) {
      await ctx.ui.select("Versions", versionLines());
    },
  });

  pi.on("session_start", async (_event, ctx) => {
    ctx.ui.setTitle("pidba");
    ctx.ui.setToolsExpanded(false);
    ctx.ui.setWorkingMessage("Analyzing");
    parserBuild = await readParserVersion();
    installCompactChrome(ctx);
    if (!rebuilt) {
      ctx.ui.notify(`Parser was not rebuilt for this session (${parserOrigin || "reason unknown"}). It may be stale; use /version for details.`, "warning");
    }
    if (parserBuild.version.includes("dev build") || parserBuild.version === "unknown") {
      ctx.ui.notify("The parser build is not identifiable. Use /version and rebuild it through the launcher.", "warning");
    }
  });

  // Pi's own prompt introduces a coding assistant with file and shell tools. This agent has
  // neither — every answer comes from mongo-log-parser through the mongo_* tools — so the prompt is
  // replaced rather than appended to, and SYSTEM.md is the whole of it.
  pi.on("before_agent_start", async () => ({
    systemPrompt: `${systemPrompt}

You are running ${parserBuild.raw}, driven by pidba ${pidbaVersion}. If the user asks which version of
anything they are using, or why an answer differs from one they remember, say this and point them at
/version; do not guess at a release number or a changelog.

${dataPath
  ? `Local bundle revision ${bundleVersion} is selected. All tools read it without a path argument. If the revision changed, discard earlier bundle findings and run a fresh report; never mix evidence from different bundles.`
  : "No local bundle is selected. Ask the user for the existing local directory containing the logs/FTDC, together with the problem. Do not call tools until a bundle is selected. Use /path if automatic selection did not recognize it."}`,
  }));

  pi.registerTool({
    name: "mongo_report",
    label: "MongoDB path report",
    description: "Read the whole path in one pass and report what stands out: what was running (server version, role, storage engine, cache size, restarts and unclean shutdowns), what the labels saw — every machine and server metric of every node read against its rule's bands and shape, sorted into the labels worth opening, the ones worth noticing, and a count of the ordinary rest — a chart of how much was logged over time split by severity, the slow query shapes that cost the most time and where that time went, replication and sharding events per node, the catalogue changes (index builds, drops, renames), the conditions the server logged without raising a severity, and the log groups worth reviewing. This is the right first call for an unexplained problem: the labels worth opening are where an investigation starts, and mongo_label opens one. Use only to keep one section when you already know which one you want.",
    parameters: Type.Object({
      only: Type.Optional(Type.Array(Type.String(), { maxItems: 9, description: "Sections to print: deployment, labels, activity, slow, replication, storage, ddl, signals, logs. Every section by default." })),
      ...windowFlags,
    }),
    async execute(_id, p, signal, _update, ctx) {
      return result(ctx, await run("report", [
        ...arg("only", (p.only ?? []).join(",")), ...windowArgs(p),
      ], signal));
    },
    ...renderer("report", (report) => {
      const labels = (report.data.labels ?? {}) as { open?: unknown[]; notice?: unknown[] };
      const shapes = (report.data.slow_queries as unknown[])?.length ?? 0;
      return `${labels.open?.length ?? 0} labels worth opening · ${labels.notice?.length ?? 0} worth noticing · ${shapes} query shapes`;
    }),
  });

  pi.registerTool({
    name: "mongo_label",
    label: "MongoDB label opened",
    description: "Open one label up on a node: the step after the report's labels section. It gives the rule behind the label (what it measures and how its bands read), the label's usual level, worst minute and worst sample, stretches of samples past its mark, longest first, with observed timestamps; elapsed durations require cadence and gap checks, and for the longest few windows: the labels the rule pairs with, read over those samples beside their usual level (the pool size beside how scarce the pool was, the wait time beside the queue), and what that node's log said in the same window — slow operations and where their time went, logged conditions, elections and step downs, index builds, error and warning counts, and the lines command that prints them. Set at to read a window sample by sample, the label and its pairs side by side. Label names are the ones the report prints, e.g. tickets.read.empty, cache.dirty_above_20, disk.await.",
    parameters: Type.Object({
      label: Type.String({ description: "The label to open, as the report printed it, e.g. tickets.read.empty" }),
      source: Type.Optional(Type.String({ description: "The node to open it on, as the report printed it. Every node that has the label by default." })),
      instance: Type.Optional(Type.String({ description: "The disk or replica set member, for a label that has them (the word after the label name in the report)" })),
      at: Type.Optional(Type.String({ description: "A moment to read second by second, same forms as from; take it from a stretch or window the label printed" })),
      span: Type.Optional(Type.String({ description: "How much around at to print second by second, e.g. 2m. 4m by default, at most 15m" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal, _update, ctx) {
      return result(ctx, await run("label", [
        p.label, ...arg("source", p.source), ...arg("instance", p.instance), ...arg("at", p.at),
        ...arg("span", p.span), ...windowArgs(p),
      ], signal));
    },
    ...renderer("label", (report) => {
      const opened = (report.data as unknown as { stretch_count?: number }[]) ?? [];
      const stretches = opened.reduce((sum, o) => sum + (o.stretch_count ?? 0), 0);
      return `${opened.length} opened · ${stretches} stretches past the mark`;
    }),
  });

  pi.registerTool({
    name: "mongo_coverage",
    label: "MongoDB bundle coverage",
    description: "What the bundle actually holds, and what it does not: every node found, what each one's logs and FTDC cover in time, which nodes are named by what evidence, and the blind spots — stretches where a node wrote no log line at all, each one labelled with what it opened and closed with, so a gap left by a crash reads differently from one left by a missing rotation. Call this first. Every other answer is bounded by what this reports: a question about a window nobody logged has no answer, and a count that looks low is often a log that was not collected rather than an event that did not happen.",
    parameters: Type.Object({}),
    async execute(_id, _params, signal, _update, ctx) {
      return result(ctx, await run("coverage", [], signal));
    },
    ...renderer("coverage", (report) => {
      const nodes = (report.data as Record<string, unknown>).nodes as unknown[] | undefined;
      const blind = (report.data as Record<string, unknown>).blind_spots as unknown[] | undefined;
      return `${nodes?.length ?? 0} nodes · ${blind?.length ?? 0} blind spots`;
    }),
  });

  pi.registerTool({
    name: "mongo_metrics",
    label: "MongoDB FTDC metrics",
    description: "Chart FTDC metrics over a window. Name metrics to see exactly those, or omit them for the standard overview board (queues, tickets, latency, operations, cache, cpu, disk), the same board for every node and every window. Name several sources to compare nodes: each stream is read over the same window and identified by its source, and the rows of one metric stay together. Set overlay to put every node's line on one chart when the question is where two nodes stopped agreeing.",
    parameters: Type.Object({
      sources: Type.Optional(Type.Array(Type.String(), { maxItems: 12, description: "Node names from mongo_coverage; several to compare nodes, or [\"all\"] for every stream. Every stream by default." })),
      metrics: Type.Optional(Type.Array(Type.String(), { maxItems: 20, description: "Metric paths, e.g. serverStatus/globalLock/currentQueue/total. * and ? match, so a subtree can be asked for whole: flowControl/*, wiredTiger/cache/*. Every column FTDC holds is reachable, not only the board's: the kernel's TCP counters are systemMetrics/netstat/Tcp:* and TcpExt:* (RetransSegs, OutSegs, TCPTimeouts, ListenOverflows, CurrEstab)" })),
      overlay: Type.Optional(Type.Boolean({ description: "Draw every stream of a metric on one monochrome chart with a legend, instead of a chart each" })),
      top: Type.Optional(Type.Integer({ minimum: 1, maximum: 40, description: "How many metrics of the standard board to draw when none are named" })),
      height: Type.Optional(Type.Integer({ minimum: 2, maximum: 20, description: "Chart rows per metric" })),
      bin: Type.Optional(Type.Integer({ minimum: 1, description: "Bin size in seconds; chosen from the window by default" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal, _update, ctx) {
      const sources = (p.sources ?? []).flatMap((source) => ["--source", source]);
      const metrics = (p.metrics ?? []).flatMap((metric) => ["--metric", metric]);
      return result(ctx, await run("metrics", [
        ...sources, ...metrics, ...arg("top", p.top), ...arg("height", p.height),
        ...arg("bin", p.bin), ...windowArgs(p),
        ...(p.overlay ? ["--overlay"] : []),
      ], signal));
    },
    ...renderer("metrics", (report) => `${(report.data as unknown as unknown[])?.length ?? 0} metrics charted`),
  });

  pi.registerTool({
    name: "mongo_logs",
    label: "MongoDB log groups",
    description: "Group every structured log line in the bundle, rotations and gzip included, and rank the groups by how much they matter: severity first, then failure signals, then event kind. A chart of log volume over time is drawn above the table, split into errors, warnings and everything else, with the severity and category totals under it. Narrow with a category, a minimum severity, a node or a substring, and pass a group id to read the lines themselves with their attributes.",
    parameters: Type.Object({
      top: Type.Optional(Type.Integer({ minimum: 1, maximum: 100, description: "How many groups to show" })),
      category: Type.Optional(Type.String({ description: "slow_query, election, initial_sync, recovery, replication, sharding, ddl, storage, network, authentication, query or other" })),
      severity: Type.Optional(Type.String({ description: "Keep this severity and worse: F, E, W or I" })),
      source: Type.Optional(Type.String({ description: "Node name from mongo_coverage: only the groups seen on that node" })),
      contains: Type.Optional(Type.String({ description: "Only the groups whose message contains this text" })),
      group: Type.Optional(Type.String({ description: "Group id from an earlier result: shows the lines themselves instead of the summary" })),
      records: Type.Optional(Type.Integer({ minimum: 1, maximum: 50, description: "How many lines to show for a group" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal, _update, ctx) {
      return result(ctx, await run("logs", [
        ...arg("top", p.top), ...arg("category", p.category), ...arg("severity", p.severity),
        ...arg("source", p.source), ...arg("contains", p.contains), ...arg("group", p.group),
        ...arg("records", p.records), ...windowArgs(p),
      ], signal));
    },
    ...renderer("logs", (report) => `${report.data.total_groups ?? 0} groups from ${(report.data.quality as Record<string, unknown>)?.records_parsed ?? 0} records`),
  });

  pi.registerTool({
    name: "mongo_slow",
    label: "MongoDB slow queries",
    description: "Rank slow query shapes by summed logged elapsed duration, with count, average, p95 and maximum duration, documents and keys examined, documents examined per document returned, bytes read, and what each shape mostly waited for. Under the table, summed timing fields across observed slow operations: flow control, write concern, lock acquire, ticket queue, storage read, remote shards, cpu, and what the log did not account for. Timing fields may overlap or be absent and do not establish cause. Always report the observed count and time bounds; a single slow execution does not establish recurrence or total workload frequency. Naming a filter — ns, plan, query_hash or waited_on — also adds a drill-down for the costliest shape it kept: a duration chart (min, average and worst per bin), an executions chart, the full distribution of every measure with min, average, p50, p95, p99, max and total, and the slowest real logged executions with the command document each one ran. That is the view to use once the ranking has told you which shape to look at, because a single average cannot tell an operation that is reliably slow from one that is usually fine with a bad tail. Query hashes are the original logged values. Rows without a hash are namespace/operation/plan groups, not known query shapes.",
    parameters: Type.Object({
      top: Type.Optional(Type.Integer({ minimum: 1, maximum: 100, description: "How many shapes to show" })),
      plan: Type.Optional(Type.String({ description: "COLLSCAN, IXSCAN, IDHACK, COUNT or EOF" })),
      ns: Type.Optional(Type.String({ description: "Namespace, e.g. shop.orders" })),
      op: Type.Optional(Type.String({ description: "Only these operations: a command name (find, aggregate, insert, update, delete, getMore, findAndModify), a comma-separated list of them, or a group: writes, reads, ddl" })),
      query_hash: Type.Optional(Type.String({ description: "Exact queryHash or planCacheShapeHash from the log" })),
      waited_on: Type.Optional(Type.String({ description: "Keep only the shapes whose biggest wait is this: flow control, write concern, lock acquire, ticket queue, storage read or remote shards" })),
      examples: Type.Optional(Type.Integer({ minimum: 0, maximum: 20, description: "How many real logged executions of the shape to show, slowest first, with the command each one ran. Defaults to 3 once a filter is given; 0 leaves them out." })),
      no_detail: Type.Optional(Type.Boolean({ description: "Leave out the drill-down a filter would otherwise add, keeping only the ranked table" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal, _update, ctx) {
      return result(ctx, await run("slow", [
        ...arg("top", p.top), ...arg("plan", p.plan), ...arg("ns", p.ns), ...arg("op", p.op),
        ...arg("query-hash", p.query_hash), ...arg("waited-on", p.waited_on),
        ...arg("examples", p.examples), ...(p.no_detail ? ["--no-detail"] : []),
        ...windowArgs(p),
      ], signal));
    },
    ...renderer("slow queries", (report) => {
      const waits = (report.data.waits as { name?: string; share?: number }[] | undefined) ?? [];
      const lead = waits.find((wait) => wait.name !== "unaccounted" && wait.name !== "cpu");
      const spent = lead ? ` · mostly ${lead.name} ${Math.round((lead.share ?? 0) * 100)}%` : "";
      return `${report.data.total_shapes ?? 0} shapes · ${report.data.collscan_shapes ?? 0} COLLSCAN${spent}`;
    }),
  });

  pi.registerTool({
    name: "mongo_lines",
    label: "MongoDB log lines",
    description: "The log lines themselves, exactly as the server wrote them, filtered by node, time, severity, component, log id, connection or thread, message text, or an attribute test. This is what every other number here is made of: use it to confirm a finding before acting on it, to read what surrounds a moment (around plus window), or to follow one connection through a failure. Prefer an attribute test over a text search when the field is known — attr: [\"ns=shop.orders\"], [\"durationMillis?\"], [\"errName~Stale\"] — and ask for count when the question is how many rather than which ones.",
    parameters: Type.Object({
      node: Type.Optional(Type.String({ description: "Node name or part of one; comma-separated for several" })),
      sev: Type.Optional(Type.String({ description: "Severities, comma-separated: F, E, W, I, D1..D5. W+ means W and worse, E+ means E and worse" })),
      comp: Type.Optional(Type.String({ description: "Components, comma-separated: NETWORK, REPL, COMMAND, STORAGE, CONTROL ..." })),
      id: Type.Optional(Type.String({ description: "Log ids, comma-separated, e.g. 51803 for a slow operation" })),
      ctx: Type.Optional(Type.String({ description: "Thread or connection, e.g. conn12345" })),
      msg: Type.Optional(Type.String({ description: "Text in the message, case-insensitive" })),
      grep: Type.Optional(Type.String({ description: "Text anywhere in the line, case-insensitive" })),
      attr: Type.Optional(Type.Array(Type.String(), { maxItems: 8, description: "Attribute tests: path=value, path~part, or path? for present" })),
      limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 200, description: "How many lines to show" })),
      newest: Type.Optional(Type.Boolean({ description: "Newest first" })),
      count: Type.Optional(Type.Boolean({ description: "Only count the matching lines, by node" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal, _update, ctx) {
      const attrs = (p.attr ?? []).flatMap((test) => ["--attr", test]);
      return result(ctx, await run("lines", [
        ...arg("node", p.node), ...arg("sev", p.sev), ...arg("comp", p.comp), ...arg("id", p.id),
        ...arg("ctx", p.ctx), ...arg("msg", p.msg), ...arg("grep", p.grep), ...attrs,
        ...arg("limit", p.limit), ...(p.newest ? ["--newest"] : []), ...(p.count ? ["--count"] : []),
        ...windowArgs(p),
      ], signal));
    },
    ...renderer("log lines", (report) => `${(report.data as Record<string, unknown>).matched ?? 0} matching lines`),
  });

  pi.registerTool({
    name: "mongo_storage",
    label: "MongoDB data size",
    description: "How much data there is and where it sits: the size of each mount the server's files are on, how full it is, and what the collector recorded for them, together with the index rebuilds that would have to re-read it. Mount utilization is an observation, not a diagnosed failure.",
    parameters: Type.Object({}),
    async execute(_id, _params, signal, _update, ctx) {
      return result(ctx, await run("storage", [], signal));
    },
    ...renderer("storage", () => "data sizes"),
  });

  pi.registerTool({
    name: "mongo_shell",
    label: "MongoDB shell printouts",
    description: "What the one-off shell outputs in the bundle say: serverStatus, rs.status(), hostInfo, getCmdLineOpts and the rest, as the collector captured them. These are a single moment, not a history — the moment the bundle was taken — so they answer what the machine and the set looked like then, which the logs never state outright. Call with no name to list what the bundle holds.",
    parameters: Type.Object({
      name: Type.Optional(Type.String({ description: "Which printout to read, e.g. serverstatus, rs_status, host_info, getcmdlineopts" })),
    }),
    async execute(_id, p, signal, _update, ctx) {
      return result(ctx, await run("shell", p.name ? [p.name] : [], signal));
    },
    ...renderer("shell output", (report) => String((report.data as Record<string, unknown>).kind ?? "shell outputs")),
  });

  pi.registerTool({
    name: "mongo_deployment",
    label: "MongoDB deployment",
    description: "What was actually running: per process the role (mongod, mongos, shard server or config server), the version it is on now and the ones it was on before, when it last started, storage engine, WiredTiger cache size, replica set, whether authorization was on, how many times it started inside the window, and whether it ever came up after an unclean shutdown. Under the table, the version each process came up on at each restart in order — which is what separates a finished upgrade from one that stalled halfway or was rolled back, and a set of version strings cannot. All of it is written once at startup, so a bundle whose logs were rotated past the last restart reports that instead of a number. Call this before reading any other figure: a queue depth means one thing on a 4 GB cache and another on 200 GB, and a counter that reset at a restart is not a counter.",
    parameters: Type.Object({ ...windowFlags }),
    async execute(_id, p, signal, _update, ctx) {
      return result(ctx, await run("deployment", windowArgs(p), signal));
    },
    ...renderer("deployment", (report) => {
      const summary = (report.data.summary as Record<string, unknown>) ?? {};
      return `${(report.data.processes as unknown[])?.length ?? 0} processes · ${((summary.versions as string[]) ?? []).join(", ") || "version not logged"}`;
    }),
  });

  pi.registerTool({
    name: "mongo_ddl",
    label: "MongoDB catalogue changes",
    description: "Index builds and catalogue changes with their timing: every build followed from the line that started it to the line that ended it, with how long it ran and whether it completed, failed, was aborted or never logged a completion — and beside them the drops, creates, renames, collMods, shardCollections and featureCompatibilityVersion changes. A build or a drop that overlapped a bad window is the cheapest explanation there is, and it appears in no metric. Filter by namespace identifier or by kind.",
    parameters: Type.Object({
      ns: Type.Optional(Type.String({ description: "Namespace, e.g. shop.orders" })),
      kind: Type.Optional(Type.String({ description: "One kind only, e.g. \"index build\", \"drop collection\", \"drop index\", \"shard collection\"" })),
      top: Type.Optional(Type.Integer({ minimum: 1, maximum: 100, description: "How many rows of each table to show" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal, _update, ctx) {
      return result(ctx, await run("ddl", [...arg("ns", p.ns), ...arg("kind", p.kind), ...arg("top", p.top), ...windowArgs(p)], signal));
    },
    ...renderer("catalog changes", (report) =>
      `${(report.data.index_builds as unknown[])?.length ?? 0} index builds · ${(report.data.changes as unknown[])?.length ?? 0} other changes`),
  });

  pi.registerTool({
    name: "mongo_health",
    label: "MongoDB logged conditions",
    description: "The conditions MongoDB logs at severity I and therefore never ranks by severity: authentication failures, write conflicts, lock timeouts, cursors reaped, operations killed, heartbeat failures, members reported unreachable, stale routing tables, jumbo chunks, connection churn, storage operations that took too long, clock skew. Each with a count, a rate per minute, when it started and stopped, how many nodes saw it, and what to look at next. Counts and rates do not establish impact or cause; review_priority is a navigation aid, not logged severity. Rows expose match patterns, match provenance and actual logged severity counts. A condition here is a pattern in the message text, not a verdict: confirm it with mongo_logs before acting on it.",
    parameters: Type.Object({
      top: Type.Optional(Type.Integer({ minimum: 1, maximum: 100, description: "How many conditions to show" })),
      ...windowFlags,
    }),
    async execute(_id, p, signal, _update, ctx) {
      return result(ctx, await run("health", [...arg("top", p.top), ...windowArgs(p)], signal));
    },
    ...renderer("health", (report) => `${(report.data.signals as unknown[])?.length ?? 0} conditions`),
  });

  pi.registerTool({
    name: "mongo_page",
    label: "Next page of a parser answer",
    description: "Read another page of a parser answer that was too big for one result. A paged answer starts with a line naming its result id and page count; it is one JSON document split at byte boundaries, so join the pages in order and read every page before concluding anything from it. Nothing in a paged answer is omitted.",
    parameters: Type.Object({
      result: Type.String({ description: "The paged_result id from the first page, e.g. r3" }),
      page: Type.Integer({ minimum: 2, description: "The page to read, from 2 to the page count" }),
    }),
    async execute(_id, p) {
      const { text, pages: total } = pages.page(p.result, p.page);
      return { content: [{ type: "text" as const, text: `${pageHeader(p.result, p.page, total)}\n${text}` }], details: {} };
    },
    renderCall(args: { result?: string; page?: number }, theme: Theme) {
      return new Text(`${theme.fg("accent", "◆")} ${theme.fg("toolTitle", `page ${args.page ?? "?"} of ${args.result ?? "?"}`)}`, 0, 0);
    },
  });
}
