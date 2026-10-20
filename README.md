# mongo-parser

Reads a MongoDB support bundle — structured logs, log exports, `diagnostic.data`, shell printouts
and configs, in any mix of directories and archives — once into a local store, and answers from
there.

What a bundle holds varies from customer to customer, so files are recognised by their content, not
their names. The MongoDB log and FTDC are the core. Beside them:

- **Log exports (CSV).** An export's time column is often local time. The zone is read from the server's
  own times inside the lines (wall times, cluster times) and the rows are moved to UTC. Rows of the
  same pod in different datacenters stay apart by the export's `dc` column.
- **Copies.** A file that is an exact copy of another is read once. A log that overlaps another one
  (the current log and the archive that rotated it) has its repeated lines read once.
- **Operating-system logs** (syslog, journal, dmesg), when present, are read for the few events that
  end a process or stall a machine: OOM kills, mongod unit starts and exits, a full file table, hung
  tasks, disk errors, boots. Their zone and year come from the logs themselves, the machine's other
  logs, or mongod's own log, and an event says when its time was assumed.
- **Restarts and machine changes** come from FTDC as well as the log: every chunk records the uptime, the
  version, the cache size, RAM and CPUs, so a restart, an upgrade or a resized machine shows even
  where no log line of it survived.

Download a binary for macOS or Linux (arm64, amd64) from the
[releases](https://github.com/ynsuy/mongo-parser/releases), or build it with Go 1.26+:

```bash
go install github.com/ynsuy/mongo-parser/cmd/mongo-parser@latest
mongo-parser "/path/to/bundle"
```

`go install` records the release it fetched as the version; a clone built with `./scripts/build.sh`
stamps it too (see [Versions](#versions)).

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

Raw customer text in JSON is unchanged and untrusted.

## Layout

Every package is under `internal/`, one directory each. They fall into four layers, each using only
the ones above it.

| Layer | Packages | What they do |
| --- | --- | --- |
| Reading the bundle | `source`, `ingest`, `store`, `ftdc`, `metrics`, `series`, `collector`, `shell`, `timeparse` | Find what a bundle holds, read every log line once into a SQLite store, decode FTDC, know what each FTDC path is and turn counters into per-second rates (`metrics`), hold a metric over a window as data (`series`), read the collector's side outputs, parse the times people type. |
| Describing it | `label` (FTDC), `analyze` (logs) | `label` turns FTDC into labels from the rules in `internal/label/rules`; `analyze` turns stored log lines into slow query shapes, replication events, catalogue changes, logged conditions and log groups. |
| Narrowing | `funnel` | The stall (every label, sorted for a first look) and an opened label (its stretches, its pairs, its windows). |
| Showing | `render`, `cli` | Terminal tables and charts, and one command per question. |
| Serving agents | `mcpserver`, `guard` | `mongo-parser mcp` serves every command as an MCP tool over stdio; `mongo-parser hook` is the PreToolUse hook that keeps file and shell tools out of the bundles this machine has stores for. |

## The rules

A rule names its FTDC paths, what one second's value is (`signal`), which summary of each minute its
bands are read on (`view`: `mean`, `max`, `min`, or `time_above` / `time_below` a `threshold`), its
fuzzy bands, the labels it `pairs_with`, and the labels that must agree before it is worth opening
(`open_with`). A burst that a minute mean would hide — a ticket pool
empty in individual samples — uses normalized valid-sample shares instead. A field MongoDB renamed is a second definition
of the same label with its own `since` / `until`.

The rules are checked when the binary starts: an unknown field, a band that leaves a gap, a time view
without a threshold or a pairing with a label that does not exist is refused. Whether the paths are
ones the server actually writes can only be checked against a real capture, of each server version
a rule changes for:

```bash
DBA_FTDC_DIR=/path/to/diagnostic.data go test ./internal/label -run TestRulesReadPathsTheServerWrites
```

## Agents

`mongo-parser mcp` is an MCP server over stdio. Every tool takes the bundle as `path` and runs
the matching command with `--json`; the answer reaches the model without its drawn text, in pages of
48 KiB (`MONGO_PARSER_MCP_PAGE_BYTES`) that `mongo_page` returns in order. The tools are marked
read-only. Add it to any MCP client by hand:

```bash
claude mcp add mongo -- /path/to/mongo-parser mcp
```

or, on the Percona Support team, install the `psb-mongo-diagnoser` plugin from the Percona Support
Bazaar, which downloads the release binary and also brings the investigation skill and the hook.

`mongo-parser hook` reads a PreToolUse payload. A Read, Grep or Bash call that would read a file
the parser read from a bundle on this machine, or a path shaped like one (`diagnostic.data`,
`mongod*.log*`), is put to the user; so is a recursive search over a directory holding such files.
Listing names (Glob, `ls`) and other files beside the bundle, such as notes in the case directory,
pass, and so does a Bash command that only runs `mongo-parser`. `MONGO_PARSER_RAW_READS=1` lets
everything through.

## The cache

A bundle is read once into a store under the user cache (`~/Library/Caches/mongo-parser` on macOS,
`~/.cache/mongo-parser` on Linux, or `MONGO_PARSER_CACHE`), and every later question answers from
it. A store runs to roughly half the bundle's size on disk, so after every new read the parser
removes the stores nobody has used for 14 days (`MONGO_PARSER_KEEP_DAYS`), those whose bundle is no
longer on the disk, and then the least recently used until the cache fits in 20 GiB
(`MONGO_PARSER_CACHE_GB`). `0` turns either off. The store just read and any used in the last hour
are kept. `mongo-parser prune` lists the stores and removes them on request.

## Versions

`scripts/build.sh` stamps the binary with `scripts/version.sh`: the nearest `vX.Y.Z` tag, or the
baseline version and a fingerprint of the sources when there is no tag. `mongo-parser version`
prints it, because a stale binary answers in the same format as a current one.

A release is the `BASELINE` in `scripts/version.sh` and the tag `vX.Y.Z`, the same. Pushing the tag
runs `.github/workflows/release.yml`, which tests, refuses a tag the baseline does not name, and
publishes the four binaries (macOS and Linux, arm64 and amd64) with their checksums. The plugin
names the release it downloads in its own `bin/parser-version`.

Time-series charts are compact Unicode bar strips (`▁▂▃▄▅▆▇█`), one bar per time bin, with a UTC axis
labelled at every row and a line of figures (min, avg, p95, max and when) per strip; tables use plain
ASCII borders. Bars stand on zero, except a series that moves less than its distance from zero
(12.2k↔13.2k), which stands on a round floor under its minimum; the axis prints the floor either way.
A blank cell is the floor; `·` on the floor is a bin with no samples. Streams of one metric share a scale.
`~` marks a shortened display name or cell; full structured values remain in JSON.
Names and log text retain their original characters. JSON data and metric calculations are unchanged.

To chart a label's derived signal (with the same formula and signal unit used by the rule):

```bash
bin/mongo-parser label /path/to/bundle disk.busy --instance sda --chart
```

Add `--source`, `--from`, `--to` or `--bin 60` to focus the graph. `--json --chart` includes
`signal_series` with sample statistics and min/avg/max time bins. For time-above/below labels,
the chart shows the underlying signal, not the normalized s/min view. Ordinary label charts plot
sample-weighted means; depletion charts retain minima. Exact sample extrema remain in the summary
and JSON. `plot_aggregation` records the selected plotting rule. Charts appear outside the statistics table,
with a UTC time axis. `metrics`, log activity and slow-query time-series use the same bar renderer.

## Develop

```bash
go vet ./... && go test ./...
./scripts/build.sh
```

To profile one command, `MONGO_PARSER_PROFILE=/tmp/prof bin/mongo-parser report /path/to/bundle`
writes `cpu.pprof` and `heap.pprof` there for `go tool pprof`.
