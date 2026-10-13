# pidba

A Pi terminal agent for MongoDB support bundles. It reads nothing itself: every answer comes from
`mongo-log-parser`, the Go tool in the directory beside this one in the same repository, which reads the bundle — logs, log
exports, FTDC, shell printouts and configs, in any mix of directories and archives — once into a
local store and answers from it. The agent reads the parser's structured answers and writes up what
happened as numbered findings with an overall assessment.

Requires Go 1.26+, Node.js 22.19.0+ and npm, plus Bash (macOS, Linux, or WSL).
Run these commands from the repository root after [cloning the repository](../README.md#install-and-run):

```bash
npm --prefix pidba ci
./pidba/scripts/pidba
```

The examples below use `pidba` as shorthand for `./pidba/scripts/pidba` from the repository root.
You can optionally link `pidba/scripts/pidba` into a directory on your PATH.
First use: type `/login`, select **OpenAI Codex**, and complete ChatGPT sign-in in the browser.
The terminal remains Pi; its built-in `openai-codex` provider runs the model through your Codex
subscription. This is not the Codex CLI and does not import its credentials.
See [OpenAI authentication documentation](https://developers.openai.com/codex/auth).

Then describe the problem with the directory in the same message:

```text
Investigate why queries slowed down in "/Users/me/Downloads/mongo bundle".
```

Absolute paths, `~/...`, `./...`, and `../...` are recognized locally. Quote paths containing
spaces. Relative paths resolve from the directory where you launched `pidba`. Alternatively use
`/path /data/mongodb`, then describe the problem. `/path` shows the selected directory.
`pidba /data/mongodb` also works. One directory per message; changing directories starts a new
bundle revision, and the agent must obtain fresh evidence.

The agent starts with the parser report, opens the labels worth opening with `mongo_label`, and
follows them down to the seconds, the slow query shapes and the log lines. It distinguishes
supporting evidence from an unconfirmed cause.

The agent has no shell and no file access at all, so no number can come from anywhere but the
parser. The model is chosen per session; the default is OpenAI through a Codex subscription.

## Codex model

The default is `openai-codex/gpt-5.5`, with medium reasoning. Override it with
`DBA_PROVIDER=<provider-id> DBA_MODEL=<model-id> pidba`, or select a model with `/model`.
Credentials live in the separate, git-ignored `.pi/codex-agent` directory.

**Model requests go to OpenAI, and the bundle's contents go with them.** The parser runs locally,
but its structured evidence — hostnames, namespaces, paths, application names, log text — is sent to the
selected model as written. There is no masking layer: an earlier version of this agent replaced
those names with local identifiers, and that was removed deliberately. Point this at a customer
bundle only where sending that customer's operational data to OpenAI is something you are allowed
to do; for anything else, run `mongo-log-parser` yourself in a terminal. `--offline` disables Pi startup update checks; it does not disable model networking.

The launcher builds the parser from `../mongo-log-parser` into this project's `bin/` directory, with
a version stamped into it. Override the source with `MONGO_LOG_PARSER_SRC`, or use an existing
executable with `MONGO_LOG_PARSER_BIN`.

`pidba --version` prints what a session would run — the launcher and Pi versions, model, full
parser build and timestamp, parser binary path, parser source checkout and build mode — without
starting one. The compact terminal chrome keeps the pidba and parser versions visible; `/version`
shows the full details and paths inside a session.

There are exactly two ways to end up running a parser that was not built at start-up: pinning one
with `MONGO_LOG_PARSER_BIN`, or pointing `MONGO_LOG_PARSER_SRC` somewhere that does not exist, in which case a
binary left in `bin/` from an earlier run is reused. Both used to be silent. Both now say so on
stderr at launch and as a warning in the session, because a months-old binary answering questions
about today's bundle looks exactly like a current one.
Building requires Go. The agent has only the parser tools listed below, with no shell or file tools.

## Tools

| Tool | What it does |
| --- | --- |
| `mongo_report` | Reads the whole path in one pass: what was running, what the labels saw (every metric of every node, sorted into worth opening, worth noticing and ordinary), log volume over time, slow query shapes and where their time went, replication and sharding events, catalogue changes, logged conditions, log groups. The first call for an unexplained problem; `only` keeps one section. |
| `mongo_label` | Opens one label on one node: the seconds it spent past its mark, the labels it pairs with over those seconds, what moved with it, and the node's log in the same window; `at` reads a window second by second. |
| `mongo_deployment` | What was running: version, role, storage engine, cache size, replica set, authorization, restarts, unclean shutdowns. Read it before reading any figure that depends on it. |
| `mongo_metrics` | Charts named FTDC metrics, or the standard overview board. Several streams can be charted together to compare nodes. Paths take `*` and `?`. |
| `mongo_slow` | Ranks slow query shapes by the time they cost, with plan, documents examined, inefficiency — and the breakdown of where the time went: flow control, write concern, lock acquire, ticket queue, storage read, remote shards, cpu. |
| `mongo_ddl` | Index builds with their durations and outcomes, and the drops, creates, renames and FCV changes around them. |
| `mongo_health` | The conditions the server logs at severity I and never ranks: authentication failures, write conflicts, heartbeat failures, connection churn, stale routing tables, storage operations that took too long. |
| `mongo_logs` | Groups every structured log line; a group id shows the lines themselves with their attributes. Filter by category, minimum severity, node or substring. |
| `mongo_coverage` | Which nodes are in the bundle, what each one's logs and FTDC cover in time, and the blind spots — stretches where a node wrote nothing, labelled with what they opened and closed with. The first call on an unfamiliar bundle. |
| `mongo_lines` | The log lines themselves, verbatim, filtered by node, time, severity, component, log id, connection, message or an attribute test. What every other number is made of. |
| `mongo_storage` | How much data there is and how full the mounts holding it are. |
| `mongo_shell` | What the collector's one-off printouts say: serverStatus, rs.status(), hostInfo, getCmdLineOpts. |

A slow operation and a busy one can print the same duration. `mongo_slow` summarizes logged timing
fields; these guide investigation but do not settle cause or exclude query/workload contributions.
Compare with FTDC only where node identity and time coverage support it.

The model receives the parser's structured evidence in `data`, with command and evidence metadata, as
JSON; the parser's rendered tables and charts stay in the terminal and do not reach the model. Tool
activity and thinking are hidden by default; expanding a tool shows the parser's own output.
No structured field is sampled or cut. An answer larger than a quarter of the model's context
window (at most 512 KiB, at least 64 KiB) arrives in pages; the first names the result and the page
count, and `mongo_page` returns the rest. The pages joined are the structured JSON byte for byte. Automatic
compaction still summarizes older turns once the session fills the window (16,384 tokens are
reserved), so a long session over a large bundle is better started fresh than compacted.

Focused slow results include structured `execution_examples` with command documents, UTC times,
source/context and logged waits. They are the slowest examples of the costliest matched group, not
representative of all operations; no defaults are inferred.

Parser descriptions and log-pattern titles report observations, not preset causes or fixes.
Log signals separate `review_priority` from actual
`log_severities` and expose pattern/provenance fields. Incident reporting includes isolated extrema
alongside main hypotheses; cited slow shapes include observed count and time bounds. A single logged
slow execution is not a total workload frequency or proof of a recurring problem. Aggregate FTDC
counters and selected log examples must not be conflated. Model conclusions still require review;
wording safeguards and unit tests are not end-to-end RCA validation.

## Comparing nodes

`mongo_metrics` takes several streams. Each is read over the same window and charted against the
same value axis, because two nodes drawn to their own scales look alike however far apart their
numbers are. The JSON identifies every stream, and a metric that appears has a row for every node.

From a terminal the same thing is:

```bash
../mongo-log-parser/bin/mongo-log-parser metrics /data/mongodb --source all \
  --metric 'wiredTiger/cache/bytes read into cache'
```

## What reaches the model

All structured evidence the parser returns for a question the agent asks: node names, namespaces, paths,
application names, query shapes with their command documents, and log message text. What you type
reaches it unchanged too. Nothing is masked or redacted on the way.

The bundle itself never leaves the machine — the parser reads it locally and the model only ever
sees the answers — but those answers are the customer's operational data. Decide once, per bundle,
whether that is allowed, rather than per question.

## Scope

Structured JSON logs (MongoDB 4.4 and later) and the Kubernetes/rsyslog exports of them. The 4.2
and older text format is recognised and deliberately not read: `coverage` names the files it skipped
so a count is never short without saying why. MySQL and PostgreSQL adapters are not written. The
parser labels; it does not diagnose. A label worth opening is a place to look, not a cause.

Query hashes are copied verbatim from the log (`queryHash` or `planCacheShapeHash`); they are never
generated. `not logged` identifies a namespace/operation/plan group without a server hash.
Ask the agent to filter by a reported query hash, or use the parser directly:

```bash
../mongo-log-parser/bin/mongo-log-parser slow /data/mongodb --query-hash ABC123EF
```

Structured `notes` preserve unavailable-label reasons and FTDC decode warnings. Parser rankings, sampled examples and time bins retain
their scope; paging preserves all returned JSON, not every raw record in the bundle.

Parser tool calls run sequentially to protect the shared cache during its first ingest. Tool errors
show the parser's actual diagnostic instead of only `failed`.

An incident answer is a list of numbered findings followed by an overall assessment. Each finding
starts with the node and says what was measured, where, when (UTC) and for how long or how many;
related labels and slow shapes of one interval are folded into one finding, and there is no fixed
number of findings. Findings describe; cause is weighed only in the overall assessment, which says
what supports it and what is missing. PIDBA draws no charts or tables of its own. Tool failures
remain visible even when tool activity is hidden.
