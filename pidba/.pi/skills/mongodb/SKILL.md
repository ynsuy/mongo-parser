---
name: mongodb-incident
description: Investigate an offline MongoDB support bundle using only the registered mongo_* tools, which are backed by the local mongo-log-parser.
---

# MongoDB incident investigation

Observed data wins over generic expectations. Keep what the logs say, what FTDC says, and what you
know about MongoDB in general as three separate things, and say which one a claim rests on.

## Start broad

`mongo_coverage` says who is in the bundle and what each node's logs and metrics actually cover.
Run it first on an unfamiliar bundle: a gap in coverage changes what every count below means, and a
question about a window nobody logged has no answer.

`mongo_report` then reads the whole bundle in one pass. Run it before any narrower tool, without a
time window unless the user gave one. Its labels are navigation summaries, not diagnoses: open them
with `mongo_label`, which shows the samples past its mark, what the labels it
pairs with did in those samples, and what the node's log said then. Check sampling cadence and gaps
before interpreting sample counts as elapsed duration. The slow query table and the
replication summary sit beside them; a category list is a navigation aid, not the set of things
that can be wrong.

Read its first section before its numbers. What was running — the version, the role, the cache size,
whether the process restarted inside the window — decides what every figure below it means, and a
bundle whose logs were rotated past the last restart will say it cannot tell you.

## Then narrow

- Slow operations, before anything about the query itself → the "where the time went" breakdown
  under `mongo_slow`. An operation that waited and one that worked print the same duration. If the
  logged timing fields show write concern, flow control or remote waits, inspect that evidence
  with `waited_on` to isolate shapes. It does not rule out query or workload contributions.
- A costly namespace or plan → `mongo_slow` with `ns`, `plan` or `query_hash`. Narrowing this way
  also returns the distribution and the charts for the costliest shape it kept. Read the spread
  before quoting an average: min, p50, p95 and max say whether a shape is reliably slow or usually
  fine with a bad tail, and those are different investigations. Min, max, count and total are exact;
  the percentiles are approximate and the parser says so. The drill-down also returns the slowest
  real executions with their command documents: read the predicate before saying anything about
  indexes, and quote the shape rather than describing it.
- A build or a drop near the window → `mongo_ddl`. Inspect logged phases and outcomes; timing
  alone does not establish that an index build caused contention.
- A condition worth a rate rather than a count → `mongo_health`. A thousand authentication failures
  a minute warrants investigation, but neither a high nor a low count establishes cause or impact.
- A metric worth seeing at full width → `mongo_metrics` with the exact metric path from a report.
- The same metric on several nodes → `mongo_metrics` with several `sources`. They are read over one
  window and share a value axis, so their magnitudes are comparable; `overlay` puts them on one
  chart. Ranking runs once across the streams, so every metric shown has a row per node.
- An event kind → `mongo_logs` with `category`, then with the `group` id to read the lines.
- A finding worth confirming, or a moment worth reading around → `mongo_lines`. A count is a claim
  about lines; this is the lines. Prefer an attribute test (`ns=shop.orders`, `errName~Stale`) over
  a text search when the field is known.
- What the machine and the set looked like when the bundle was taken → `mongo_shell`, and
  `mongo_storage` for how full the mounts are.

Reuse the names a report gave you. Do not repeat `mongo_coverage` once the nodes are known, and do
not repeat a call with arguments you have already used unless compaction removed required evidence.

## Focused requests

"Show the tickets between these two times" is not an incident investigation. Retrieve the requested
metric for the requested window, draw a plain ASCII chart or table from the returned structured
values, and stop. Run a full RCA only
when asked for one.

## Correlating

For an incident, line up independent signals by timestamp and node: slow query shapes, the metric
that moved, the replication events, the catalogue changes, and the minute the errors peaked in the
log activity chart. Similar timing supports correlation; it does not prove cause.

Where `mongo_report` has already measured a coincidence between two histories, that measurement is
the evidence — quote its share and its window rather than restating the halves, and do not attempt
one of your own from numbers in two tables. A version history from `mongo_deployment` belongs in the
same line-up: nearby rollback and stepdown events may be related, but keep their identities distinct
until evidence establishes the relationship.

Avoid double-counting related evidence. Logged flow-control wait and an FTDC rate-limit metric
measure different aspects of a mechanism; compare them only with confirmed node and time alignment.

A node's identity is settled when the bundle is read: rotations of one log and the FTDC written
beside them are tied to the node the lines name themselves after, and `mongo_coverage` says on what
evidence. Two nodes that happen to share a short hostname stay two nodes. A coverage window can
still contain gaps, so check `mongo_coverage` before correlating across nodes.

## Reporting

Use structured `data` for numbers and descriptions. Parser tables and charts are displayed locally
in the terminal; tool content contains structured JSON only. A paged answer is read whole, every
page through `mongo_page`, before anything is concluded from it.

Support answers with compact plain ASCII charts in fenced code blocks or tables when they make
comparisons or timing clearer. Use only returned structured values; do not use ANSI colors,
Unicode block sparklines or decorative drawing characters. Label the source/node, metric, unit,
UTC time bounds, bin size and aggregation (min, avg or max). Preserve zeros, isolated extrema and
missing-data gaps; never interpolate gaps or invent points. For low=true series use bucket minima
to show depletion; otherwise choose and label the aggregation that answers the question. Use a
shared time window and value scale when comparing nodes, with distinct ASCII symbols and a legend.
A visual is a summary: retain exact relevant values, timestamps, counts and coverage in text or a
table. If the returned data cannot support a time chart, use a table or retrieve the missing data.

Read `notes` and quality/coverage fields. Rankings, retained examples and time bins are scoped
summaries; retrieve narrower evidence when omitted rows or sample detail matter. After compaction,
retrieve required pages again or rerun a tool rather than drawing precise conclusions from a
conversation summary.

State what is missing when the evidence stops short of a cause, and name the artifact that would
settle it.

Review both open and notice findings and retain isolated observed extrema in a compact inventory,
even when they are not the main hypothesis. State samples/counts and timestamps, not inferred
continuous duration from a binned chart. For a cited slow shape, always include its observed count
and grouping basis; one observed slow execution does not establish recurrence or the total workload
frequency. Aggregate FTDC counters and a selected slow-log example are different populations.
Keep ranked findings separate from causal reasoning and from remediation recommendations.

Read structured `execution_examples` and their commands before declaring an operation setting
unknown. Examples describe sampled executions, not every operation or a default. When the one you
need is not among them, retrieve narrower examples or raw lines and distinguish not retrieved from
not present. Before finalizing, reconcile each returned open/notice row by label,
source and instance with a reported observation or explicit deferral. State explicitly when a
cited execution precedes/follows FTDC coverage and cannot be correlated with those metrics.
