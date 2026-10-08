---
name: mongo-bundle
description: Investigate an offline MongoDB support bundle — structured logs, log exports, diagnostic.data (FTDC), shell printouts and configs — with the local mongo-log-parser binary. Use when asked why a MongoDB deployment was slow, stalled, lagged, ran out of tickets, elected, or failed, and the evidence is a bundle on disk rather than a live server. Triggers on "look at this bundle", "diagnostic.data", "FTDC", "mongod.log", "why were queries slow", "ticket exhaustion", "replication lag in this bundle". Not for a live server, for MySQL or PostgreSQL, or for 4.2-and-older text logs, which the parser recognises and does not read.
---

# MongoDB bundle investigation

Every fact about the bundle comes from `mongo-log-parser`. Do not open, grep, or decompress the
bundle's files yourself: the parser reads them once into a local store, knows which node each file
belongs to, decodes FTDC, and says what it could not read. A number you computed from a raw file has
none of that behind it.

## Before the first command

`mongo-log-parser version` must print a line starting `mongo-log-parser v0.6` or later. If the
command is missing, or prints an older version, build it from this repository:
`mongo-log-parser/scripts/build.sh` (needs Go) and use `mongo-log-parser/bin/mongo-log-parser`.
A stale binary answers in the same format as a current one, which is why the version is checked.

## The funnel

An investigation narrows from every label on every node to the seconds and log lines behind one of
them. The labels decide where it starts.

1. **What the bundle holds.** `mongo-log-parser coverage <bundle>`: the nodes, what each one's logs
   and FTDC cover in time, and the blind spots. A question about a window nobody logged has no
   answer, and a low count may be a log nobody collected.
2. **The stall.** `mongo-log-parser report <bundle>`. Read "What was running" before any figure: a
   version decides which explanation is possible and a restart resets every counter. Then "What the
   labels saw": the labels worth opening, the ones worth noticing, and a count of the ordinary rest.
   Each row has the label, the node, its worst minute and worst second, when, why it is listed, and
   what moved with it. Several labels starting in the same minutes are one story, not several.
3. **Open the label.** `mongo-log-parser label <bundle> <label> --source <node>` (add `--instance`
   for a disk or a member). It prints the stretches of seconds the label was past its mark, with
   exact times; the labels its rule pairs with, read over those same seconds beside their usual
   level; and what that node's log said in the window — slow operations and where their time went,
   logged conditions, elections, index builds. This is where cause candidates come from.
4. **The seconds.** Add `--at <time> --span 2m` to read a stretch second by second, the label and
   its pairs side by side.
5. **The lines.** The opened label prints the `lines` command for its window. `slow` narrows a
   namespace, plan, query hash or wait (`--waited-on`); `ddl`, `health` and `metrics` cover what no
   label reads.

Every command takes `--from`/`--to`, `--around`/`--window` or `--last`, in UTC. Pass a window only
when the user gave one or a label printed it. `--json` gives the same answer as data.

## Reading labels

A label is a measurement with a name, read against its rule's fuzzy bands: a value near an edge
belongs to two bands by degree. Some labels count seconds past a threshold (`s/min`) instead of
averaging the minute, because a minute mean turns ten seconds of an empty ticket pool into an
unremarkable quarter. A ticket pool is read four ways: in use (%), seconds with under 10% of the pool
left, seconds with none left, and the pool's own size — from 7.0 the server may resize it, and
scarcity is measured against the pool as it was that second.

`mongo-log-parser labels` is the rule book: what each label is computed from, which summary of the
minute it is read on, its bands, and what it pairs with. `references/labels.md` says how to read the
rule book's columns.

## What to say

- Quote the label and its numbers behind every claim drawn from FTDC, and the log evidence behind
  every claim drawn from logs.
- "Moved with" and the pairs are correlation the parser measured, not cause. Say which moved first
  when the parser says so, and never compute a correlation of your own from two separate outputs.
- A slow operation is not a busy one: read where its time went (write concern, flow control, ticket
  queue, storage read, remote shards) before naming a query as the problem.
- A label that is not listed is ordinary or not labelled, and the report says which. An absent label
  is unknown, not quiet. When the evidence does not reach a cause, say what is missing.
- Relay the parser's tables as it drew them; add your reading in words beside them.
