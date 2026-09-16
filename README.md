# pidba

A Pi terminal agent for MongoDB logs and FTDC data. It reads nothing itself: every answer comes from
[`dba-mongo-parser`](https://github.com/ynsuy/dba-mongo-parser), a standalone Go tool that parses the bundle and draws its
own tables and charts. The agent runs the parser, shows what it drew, and reasons about it in words.

```bash
npm install
scripts/pidba
```

Open with `pidba` once `scripts/pidba` is linked into a directory on your PATH.
First use: type `/login`, select **OpenAI Codex**, and complete ChatGPT sign-in in the browser.
The terminal remains Pi; its built-in `openai-codex` provider runs the model through your Codex
subscription. This is not the Codex CLI and does not import its credentials.
See [OpenAI authentication documentation](https://developers.openai.com/codex/auth).

Then describe the problem with the directory in the same message:

```text
"/Users/me/Downloads/mongo bundle" içinde sorgular yavaşlamış. Sebebini araştır.
```

Absolute paths, `~/...`, `./...`, and `../...` are recognized locally. Quote paths containing
spaces. Relative paths resolve from the directory where you launched `pidba`. Alternatively use
`/path /data/mongodb`, then describe the problem. `/path` shows the selected directory.
`pidba /data/mongodb` also works. One directory per message; changing directories starts a new
bundle revision, and the agent must obtain fresh evidence.

The agent starts with the parser report and follows the relevant logs, FTDC metrics and slow
query shapes. It distinguishes supporting evidence from an unconfirmed cause.

## Codex model

The default is the installed provider catalog's `gpt-5.5`, with medium reasoning. Override it with
`DBA_MODEL=<model-id> pidba`, or select a model with `/model`. Credentials live in the separate,
git-ignored `.pi/codex-agent` directory. Ollama is not needed.

**Model requests go to OpenAI.** The parser runs locally with identity masking enabled; the
selected directory is replaced locally before the message is sent. Masking is partial, and pasted
attachments or unrecognized names may contain identifying information. `--offline` disables Pi
startup update checks; it does not disable model networking.

The launcher builds the parser from `../dba-mongo-parser` into this project's `bin/` directory, with
a version stamped into it. Override the source with `DBA_PARSER_SRC`, or use an existing executable
with `DBA_PARSER_BIN`.

`pidba --version` prints what a session would run — the launcher, the parser build and its
timestamp, where that binary came from, and pi — without starting one. The same three are in the
status bar of every session and behind `/version`.

There are exactly two ways to end up running a parser that was not built at start-up: pinning one
with `DBA_PARSER_BIN`, or pointing `DBA_PARSER_SRC` somewhere that does not exist, in which case a
binary left in `bin/` from an earlier run is reused. Both used to be silent. Both now say so on
stderr at launch and as a warning in the session, because a months-old binary answering questions
about today's bundle looks exactly like a current one.
Building requires Go. The agent has only the parser tools listed below, with no shell or file tools.

## Tools

| Tool | What it does |
| --- | --- |
| `mongo_report` | Reads the whole path in one pass: what was running, findings, log volume over time, a chart per FTDC stream, slow query shapes and where their time went, replication and sharding events, catalogue changes, logged conditions, log groups. The first call for an unexplained problem; `only` keeps one section. |
| `mongo_deployment` | What was running: version, role, storage engine, cache size, replica set, authorization, restarts, unclean shutdowns. Read it before reading any figure that depends on it. |
| `mongo_metrics` | Charts named FTDC metrics, or ranks the performance-relevant ones. Several streams can be charted together to compare nodes. Paths take `*` and `?`. |
| `mongo_slow` | Ranks slow query shapes by the time they cost, with plan, documents examined, inefficiency — and the breakdown of where the time went: flow control, write concern, lock acquire, ticket queue, storage read, remote shards, cpu. |
| `mongo_ddl` | Index builds with their durations and outcomes, and the drops, creates, renames and FCV changes around them. |
| `mongo_health` | The conditions the server logs at severity I and never ranks: authentication failures, write conflicts, heartbeat failures, connection churn, stale routing tables, storage operations that took too long. |
| `mongo_logs` | Groups every structured log line; a group id shows the lines themselves with their attributes. Filter by category, minimum severity, node or substring. |
| `mongo_inventory` | Lists the FTDC streams and log files the bundle holds. |

A slow operation and a busy one print the same duration. `mongo_slow` says which it was: when the
time went on write concern, flow control or a remote shard, the query is not the problem, and the
FTDC side of the same mechanism is `serverStatus.flowControl` in `mongo_metrics`.

## Comparing nodes

`mongo_metrics` takes several streams. Each is read over the same window and charted against the
same value axis, because two nodes drawn to their own scales look alike however far apart their
numbers are. Every stream gets a colour of its own and a legend line names them. Ranking is done
once across the streams rather than per stream, so a metric that appears has a row for every node
instead of one node's busiest metric sitting next to another node's.

By default each stream gets its own chart under a shared axis. `overlay` puts them on one chart,
which is the view for finding the moment two nodes stopped agreeing. Where lines cross, the one
listed first in the legend keeps the cell.

From a terminal the same thing is:

```bash
../dba-mongo-parser/bin/dba-mongo-parser metrics /data/mongodb --source all \
  --metric serverStatus/wiredTiger/cache/bytes read into cache --overlay
```

For looking around yourself rather than asking, the parser has an interactive mode the agent does
not use: `dba-mongo-parser explore <path> --source all` opens the metrics in the terminal, where
`+`/`-` zoom the window, the arrows pan it, `j`/`k` change metric and `?` lists the keys.

## Local identities

Every parser call runs with `--obscure`, which masks supported identity fields with identifiers
(`source-002`, `ns-004`, `node-011`) wherever the parser prints them, findings included. This is
partial masking: free-form log attributes, error text and pasted attachments may still contain
customer information. Rendered reports are
sent to the selected Codex model through OpenAI. The mapping lives on this machine, under
`~/.local/state/dba-mongo-parser/`, and is keyed by path: the same collection keeps the same id
across calls and sessions, and the model can hand an id back wherever a name is taken. `/aliases`
shows the mapping in the terminal, and `/aliases ns-004` looks up just the one an answer mentioned.
Terms work in both directions, so a collection you already have in mind finds the identifier the
model was given for it.

What you type is masked with the same identifiers before the model sees it. Only names that carry a
separator — a namespace, a host, a path — are substituted into your sentence, so an ordinary word
that happens to be the name of the directory you passed does not rewrite the question you asked.

Masking is not something the agent opts into. `scripts/pidba` exports `DBA_OBSCURE=1`, and the
parser obeys that over anything on its command line: no edit to an extension can put a real
hostname, path or namespace in front of the model. Running the parser yourself, without that
variable, shows the real names — that is the difference between the two ways in.

Limits worth knowing: attachments you paste are not anonymized, and an id says nothing about what it
stands for, so the model cannot reason about a name it was never given.

## Scope

Structured JSON logs only (MongoDB 4.4 and later). MySQL and PostgreSQL adapters are not written.
The parser screens; it does not diagnose. A finding is a place to look, not a cause.

Query hashes are copied verbatim from the log (`queryHash` or `planCacheShapeHash`); they are never
aliased or generated. `not logged` identifies a namespace/operation/plan group without a server hash.
Ask the agent to filter by a reported query hash, or use the parser directly:

```bash
../dba-mongo-parser/bin/dba-mongo-parser slow /data/mongodb --query-hash ABC123EF
../dba-mongo-parser/bin/dba-mongo-parser aliases /data/mongodb ns-004
```
