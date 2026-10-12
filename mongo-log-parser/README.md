# mongo-log-parser

Reads a MongoDB support bundle — structured logs, log exports, `diagnostic.data`, shell printouts
and configs, in any mix of directories and archives — once into a local store, and answers from
there.

Requires Go 1.26+ and Bash on macOS, Linux, or WSL. From a fresh clone, enter the parser directory:

```bash
git clone https://github.com/ynsuy/pidba.git
cd pidba/mongo-log-parser
scripts/build.sh                                  # a stamped binary in bin/
bin/mongo-log-parser /path/to/bundle              # the overall report
bin/mongo-log-parser coverage /path/to/bundle     # what the bundle holds, and its blind spots
bin/mongo-log-parser label /path/to/bundle tickets.read.empty --source rs0-1
bin/mongo-log-parser labels                       # the rule book
```

## How an investigation narrows

Labels help navigation. Metrics covered by the catalogue are read against a definition — a YAML file in
`internal/label/rules` — and described by its level, its shape and when it changed. The report lays
them out as worth opening, worth noticing, and ordinary. `label` opens one: observed samples past
its reference, the labels its definition pairs with over those same samples, measured correlations,
and what the node's log said in the window. `--at` reads a window second by second, and `lines`
prints the log lines themselves.

The parser labels and does not diagnose. Why something happened is for whoever reads the labels
beside the logs.

Descriptions report measurements, derived values and their support, never preset causes or fixes.
Numeric bands, thresholds and review priorities are fixed navigation references, not incident
severity or discovered server limits. Label IDs name the measurement and the fixed number it is
read against (`cpu.busy_above_95`, `cache.fill_above_95`), not a condition of the server.
Time views report normalized valid-sample shares, not continuous elapsed durations. Log signals
report text-pattern matches with match conditions, message/attribute provenance and actual logged
severity counts. `review_priority` replaces the former parser-assigned signal `severity` field;
it is a sorting aid, not MongoDB severity or customer impact. Band names are descriptive magnitude
terms; numeric membership and ranking thresholds are unchanged.

Every JSON envelope carries `evidence_policy`; raw customer text remains unchanged and untrusted.

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
fuzzy bands, the labels it `pairs_with`, and the labels that must agree before it is worth opening
(`open_with`). A burst that a minute mean would hide — a ticket pool
empty in individual samples — uses normalized valid-sample shares instead. A field MongoDB renamed is a second definition
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
there is no git. `mongo-log-parser version` prints it; pidba displays it, because a
stale binary answers in the same format as a current one.

Terminal charts use ASCII strokes and axes; tables use plain ASCII borders. Color is off by default
(`--color never`). Explicit `--color auto` or `--color always` still enables styling where supported.
Multiple series have distinct strokes and a legend; `. = gap` identifies missing data rather than zero.
`~` marks a shortened display name or cell; full structured values remain in JSON.
Names and log text retain their original characters. JSON data and metric calculations are unchanged.

The label review workbook includes supporting-label conditions (`open_with`), signal thresholds,
view/direction and a **Raw definitions** sheet containing every current definition as complete JSON.
