---
name: mongodb-incident
description: Investigate an offline MongoDB support bundle using only the registered mongo_* tools, which are backed by the local mongo-log-parser.
---

# MongoDB incident investigation

Keep what the logs say, what FTDC says, and what you know about MongoDB in general apart, and say
which one a claim rests on.

## Start broad

`mongo_report` reads the whole bundle in one pass; run it first, without a time window unless the
user gave one. On an unfamiliar bundle, `mongo_coverage` tells you which nodes are present and what
their logs and FTDC cover — a gap changes what every count means.

## Then narrow

- A label in `open` or `notice` → `mongo_label` with its exact source and instance; `at` for the
  seconds of a window.
- Slow operations → `mongo_slow`. Read where the time went before anything about the query; filter
  with `waited_on`, `ns`, `plan` or `query_hash`. A filter adds the shape's distribution (min, p50,
  p95, max) and its slowest executions with their command documents — read the predicate before
  saying anything about indexes.
- A build or drop near the window → `mongo_ddl`.
- A condition that matters as a rate → `mongo_health`.
- A metric no label reads, or one metric across nodes → `mongo_metrics` with the exact path and
  several `sources`.
- An event kind → `mongo_logs` with `category`, then the `group` id for its lines.
- Confirming a finding or reading around a moment → `mongo_lines`; prefer an attribute test
  (`ns=shop.orders`, `errName~Stale`) over a text search.
- The machine and the set at collection time → `mongo_shell`; disk usage → `mongo_storage`.

Reuse the names a result gave you, and do not repeat a call with arguments you already used.

## Focused requests

"Show the tickets between these two times" is not an incident investigation: retrieve that metric
for that window and explain the returned values briefly. Run a full investigation only when asked.
