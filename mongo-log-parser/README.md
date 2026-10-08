# mongo-log-parser

Reads a MongoDB support bundle — structured logs, log exports, `diagnostic.data`, shell printouts
and configs, in any mix of directories and archives — once into a local store, and answers from
there.

```bash
scripts/build.sh                                  # a stamped binary in bin/
bin/mongo-log-parser /path/to/bundle              # the overall report
bin/mongo-log-parser coverage /path/to/bundle     # what the bundle holds, and its blind spots
bin/mongo-log-parser label /path/to/bundle tickets.read.empty --source rs0-1
bin/mongo-log-parser labels                       # the rule book
```

## How an investigation narrows

The labels are where it starts. Every metric of every node is read against a rule — a YAML file in
`internal/label/rules` — and described by its level, its shape and when it changed. The report lays
them out as worth opening, worth noticing, and ordinary. `label` opens one: the seconds it spent past
its mark, the labels its rule pairs with read over those same seconds, the labels that moved with it,
and what the node's log said in the window. `--at` reads a window second by second, and `lines`
prints the log lines themselves.

The parser labels and does not diagnose. Why something happened is for whoever reads the labels
beside the logs.

## Layout

Every package is under `internal/`, one directory each. They fall into four layers, each using only
the ones above it.

| Layer | Packages | What they do |
| --- | --- | --- |
| Reading the bundle | `source`, `ingest`, `store`, `ftdc`, `metrics`, `series`, `collector`, `shell`, `timeparse` | Find what a bundle holds, read every log line once into a SQLite store, decode FTDC, know what each FTDC path is and turn counters into per-second rates (`metrics`), hold a metric over a window as data (`series`), read the collector's side outputs, parse the times people type. |
| Describing it | `label` (FTDC), `analyze` (logs) | `label` turns FTDC into labels from the rules in `internal/label/rules`; `analyze` turns stored log lines into slow query shapes, replication events, catalogue changes, logged conditions and log groups. |
| Narrowing | `funnel` | The stall (every label, sorted for a first look) and an opened label (its stretches, its pairs, its windows). |
| Showing | `render`, `cli` | Terminal tables and charts, and one command per question. |

## The rules

A rule names its FTDC paths, what one second's value is (`signal`), which summary of each minute its
bands are read on (`view`: `mean`, `max`, `min`, or `time_above` / `time_below` a `threshold`), its
fuzzy bands, and the labels it `pairs_with`. A burst that a minute mean would hide — a ticket pool
empty for ten seconds — is counted in seconds instead. A field MongoDB renamed is a second definition
of the same label with its own `since` / `until`.

The rules are checked when the binary starts: an unknown field, a band that leaves a gap, a time view
without a threshold or a pairing with a label that does not exist is refused. Whether the paths are
ones the server actually writes can only be checked against a real capture:

```bash
DBA_FTDC_DIR=/path/to/diagnostic.data go test ./internal/label -run TestRulesReadPathsTheServerWrites
```

Every rule, its signal, its bands and what it pairs with is laid out for review in
`../docs/labels-review.xlsx`; `scripts/labels-review.py` regenerates it from the built binary.

## Versions

`scripts/build.sh` stamps the binary with `scripts/version.sh`: the nearest `mongo-log-parser/vX.Y.Z`
tag and how far past it the tree is, or the baseline version and a fingerprint of the sources when
there is no git. `mongo-log-parser version` prints it; the plugin and pidba both check it, because a
stale binary answers in the same format as a current one.
