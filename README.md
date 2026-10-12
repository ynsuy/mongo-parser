# MongoDB support bundle analysis

Reads an offline MongoDB support bundle — structured logs, log exports, `diagnostic.data` (FTDC),
shell printouts and configs, in any mix of directories and archives — and narrows from what every
metric of every node did to the seconds and log lines behind the one that matters.

```text
mongo-log-parser/   the Go tool that does all the reading: labels, the funnel, the log analysis
pidba/              a Pi terminal agent that drives the tool with no shell of its own
docs/               labels-review.xlsx, every label rule laid out for review
```

## How an investigation narrows

1. **Labels.** Metrics covered by the catalogue are read against definitions (`mongo-log-parser/internal/label/rules`)
   and described by their values, shapes and observed times. Time views summarize valid-sample shares,
   normalized to s/min, not continuous elapsed durations. The report lays the labels out as
   worth opening, worth noticing, and ordinary.
2. **One label opened.** The observed samples past its reference, other labels over those same
   timestamps, measured correlations, and available matched-node logs.
3. **The seconds and the lines.** The window second by second, then the log lines themselves.

The parser reports observations and derived summaries, not preset causes or fixes. Reference bands
and review priorities are navigation, not incident severity. Unlabelled or missing evidence remains
unknown. The analyst or AI evaluates hypotheses using node/time-aligned evidence and states what
cannot be established; neither an isolated maximum nor a dominant logged wait establishes cause.

## Install and run

Use macOS, Linux, or Windows through WSL with Bash. Install Git and Go 1.26 or later.
For PIDBA, also install Node.js 22.19.0 or later and npm. The first build/install needs internet
access to download Go and npm dependencies; you do not need a running MongoDB server.

```bash
git clone https://github.com/ynsuy/pidba.git
cd pidba
```

### Parser only

The parser runs locally and needs no model account, API key, Node.js, or Ollama:

```bash
./mongo-log-parser/scripts/build.sh
./mongo-log-parser/bin/mongo-log-parser --version
./mongo-log-parser/bin/mongo-log-parser coverage "/path/to/bundle"
./mongo-log-parser/bin/mongo-log-parser "/path/to/bundle"
./mongo-log-parser/bin/mongo-log-parser labels
```

Terminal charts and table borders use ASCII, with colors off by default. Missing samples are shown
as gaps. `--json` returns structured data plus a `render` field for terminal presentation.
Open a reported label with its exact node name:

```bash
./mongo-log-parser/bin/mongo-log-parser label "/path/to/bundle" tickets.read.empty --source rs0-1
./mongo-log-parser/bin/mongo-log-parser label "/path/to/bundle" tickets.read.empty --source rs0-1 --at 10:00:00 --span 2m
```

### PIDBA terminal agent

From the repository root, install the locked dependencies and start the launcher:

```bash
npm --prefix pidba ci
./pidba/scripts/pidba --version
./pidba/scripts/pidba
```

The launcher builds the parser automatically. On first use, type `/login`, select **OpenAI Codex**,
and complete sign-in with an account that has access. Then enter a problem and your bundle path,
for example `Investigate query latency in "/path/to/bundle"`. You can also launch with
`./pidba/scripts/pidba "/path/to/bundle"`. See [PIDBA setup and tools](pidba/README.md).

PIDBA sends structured parser results and your messages to the selected model. Tables and charts
stay in the terminal; the model can support its answers with ASCII charts or tables based on JSON.
Operational names, query commands and log text are not masked. The parser-only commands above
analyze locally without model requests.

The [label review workbook](docs/labels-review.xlsx) contains 74 label names across 91 definitions,
including version ranges, bands, thresholds, supporting-label conditions and complete raw JSON.
Python is needed only if you want to regenerate that workbook, not to use the parser or PIDBA.

## Develop

```bash
(cd mongo-log-parser && go vet ./... && go test ./...)
(cd pidba && npm ci && npm run verify)
(cd mongo-log-parser && DBA_FTDC_DIR=/path/to/diagnostic.data go test ./internal/label -run TestRulesReadPathsTheServerWrites)
# Optional: regenerate the Excel workbook from a freshly built parser.
python3 -m venv /tmp/pidba-labels-venv
/tmp/pidba-labels-venv/bin/pip install openpyxl
./mongo-log-parser/scripts/build.sh
/tmp/pidba-labels-venv/bin/python mongo-log-parser/scripts/labels-review.py
```

The last test checks that every rule reads FTDC paths a real capture carries; run it against a
capture of each server version a rule changes for. The parser is versioned by tags named
`mongo-log-parser/vX.Y.Z`; pidba by its `package.json`.
