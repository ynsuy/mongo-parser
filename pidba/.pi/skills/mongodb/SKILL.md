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
time window unless the user gave one. Its labels worth opening decide where to look next: open them
with `mongo_label`, which says exactly which seconds a label was past its mark, what the labels it
pairs with did in those seconds, and what the node's log said then. The slow query table and the
replication summary sit beside them; a category list is a navigation aid, not the set of things
that can be wrong.

Read its first section before its numbers. What was running — the version, the role, the cache size,
whether the process restarted inside the window — decides what every figure below it means, and a
bundle whose logs were rotated past the last restart will say it cannot tell you.

## Then narrow

- Slow operations, before anything about the query itself → the "where the time went" breakdown
  under `mongo_slow`. An operation that waited and one that worked print the same duration. If the
  time went on write concern, flow control or remote shards, the query is not the problem and
  examining it further is wasted work; follow the wait instead, with `waited_on` to isolate the
  shapes it applies to.
- A costly namespace or plan → `mongo_slow` with `ns`, `plan` or `query_hash`. Narrowing this way
  also returns the distribution and the charts for the costliest shape it kept. Read the spread
  before quoting an average: min, p50, p95 and max say whether a shape is reliably slow or usually
  fine with a bad tail, and those are different investigations. Min, max, count and total are exact;
  the percentiles are approximate and the parser says so. The drill-down also returns the slowest
  real executions with their command documents: read the predicate before saying anything about
  indexes, and quote the shape rather than describing it.
- A build or a drop near the window → `mongo_ddl`. An index build holds a very visible lock at the
  end of its life, and nothing in FTDC names it.
- A condition worth a rate rather than a count → `mongo_health`. A thousand authentication failures
  a minute is a client misconfiguration; ten over two days is nothing.
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
not repeat a call with arguments you have already used.

## Focused requests

"Show the tickets between these two times" is not an incident investigation. Retrieve the requested
metric for the requested window, show the chart the parser returned, and stop. Run a full RCA only
when asked for one.

## Correlating

For an incident, line up independent signals by timestamp and node: slow query shapes, the metric
that moved, the replication events, the catalogue changes, and the minute the errors peaked in the
log activity chart. Similar timing supports correlation; it does not prove cause.

Where `mongo_report` has already measured a coincidence between two histories, that measurement is
the evidence — quote its share and its window rather than restating the halves, and do not attempt
one of your own from numbers in two tables. A version history from `mongo_deployment` belongs in the
same line-up: a rollback and a stepdown storm in the same minute are one event, not two.

Two signals that describe the same mechanism are one observation, not two. Flow control in the slow
query breakdown and `flowControl.targetRateLimit` in FTDC are the same thing seen from two files;
report them as one finding with two sources, not as corroboration.

A node's identity is settled when the bundle is read: rotations of one log and the FTDC written
beside them are tied to the node the lines name themselves after, and `mongo_coverage` says on what
evidence. Two nodes that happen to share a short hostname stay two nodes. A coverage window can
still contain gaps, so check `mongo_coverage` before correlating across nodes.

## Reporting

Include the parser's chart verbatim in a fenced code block when it supports the point. It is compact
plain text, not an image; keep its scale, timestamps, names and values exactly as returned.

State what is missing when the evidence stops short of a cause, and name the artifact that would
settle it.
