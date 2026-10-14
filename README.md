# MongoDB FTDC and log diagnoser

Reads an offline MongoDB support bundle — structured logs, log exports, `diagnostic.data` (FTDC),
shell printouts and configs, in any mix of directories and archives — and narrows from what every
metric of every node did to the seconds and log lines behind the one that matters.

```text
mongo-log-parser/   the Go tool that does all the reading: labels, the funnel, the log analysis
plugin/             mongo-diagnoser, the Claude Code and Codex plugin: the parser's MCP tools, the
                    investigation skill, and a hook that keeps raw bundle reads behind the user
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

**Claude Code or Codex plugin** (no Go, Node or clone needed; the plugin downloads the parser
release it was published with on first use, into `~/.cache/mongo-diagnoser`):

```bash
# Claude Code
claude plugin marketplace add ynsuy/pidba
claude plugin install mongo-diagnoser@psb-mongo

# Codex
codex plugin marketplace add ynsuy/pidba
codex plugin add mongo-diagnoser@psb-mongo
```

Then ask about a bundle by its path: `Investigate why queries slowed down in ~/cases/CS123`. In
Claude Code, `/mongo-diagnoser:investigate` starts the same skill explicitly. Codex runs plugin
hooks only after you trust them in `/hooks`.

The hook stops file and shell tools from reading a bundle directly, so every figure comes from
the parser. Claude Code asks you first; Codex cannot ask from a hook, so it refuses, and
`MONGO_LOG_PARSER_RAW_READS=1` in the environment allows raw reads for that session.

**PIDBA** (requires Git, Bash, Go 1.26+, Node.js 22.19.0+ and npm; builds the parser automatically):

```bash
git clone https://github.com/ynsuy/pidba.git
cd pidba
npm --prefix pidba ci
./pidba/scripts/pidba
```

On first use: `/login` → **OpenAI Codex** → sign in. Then describe the problem with your bundle path.

**Parser only** (Go 1.26+, local, no model account needed):

```bash
./mongo-log-parser/scripts/build.sh
./mongo-log-parser/bin/mongo-log-parser "/path/to/bundle"
```

More: [PIDBA](pidba/README.md), [parser](mongo-log-parser/README.md), [label workbook](docs/labels-review.xlsx).

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

To profile one command, `MONGO_LOG_PARSER_PROFILE=/tmp/prof mongo-log-parser/bin/mongo-log-parser
report /path/to/bundle` writes `cpu.pprof` and `heap.pprof` there for `go tool pprof`.

The last test checks that every rule reads FTDC paths a real capture carries; run it against a
capture of each server version a rule changes for. The parser is versioned by tags named
`mongo-log-parser/vX.Y.Z`; pidba by its `package.json`.

To try the plugin against a local parser build, point the launcher at it (Claude Code passes the
environment to MCP servers; Codex does not, so copy the build to
`~/.cache/mongo-diagnoser/mongo-log-parser-vX.Y.Z` instead):

```bash
MONGO_LOG_PARSER_BIN="$PWD/mongo-log-parser/bin/mongo-log-parser" claude --plugin-dir ./plugin
```

A release is the version in `plugin/.claude-plugin/plugin.json` and `plugin/plugin.json`, the
`BASELINE` in `mongo-log-parser/scripts/version.sh`, and the tag `mongo-log-parser/vX.Y.Z`, all the
same. Pushing the tag runs `.github/workflows/release-parser.yml`, which tests, builds the four
binaries the plugin downloads (macOS and Linux, arm64 and amd64) with their checksums, and refuses
a tag the plugin manifest does not name.
