# MongoDB support bundle analysis

Reads an offline MongoDB support bundle — structured logs, log exports, `diagnostic.data` (FTDC),
shell printouts and configs, in any mix of directories and archives — and narrows from what every
metric of every node did to the seconds and log lines behind the one that matters.

```text
mongo-log-parser/   the Go tool that does all the reading: labels, the funnel, the log analysis
plugin/             a Claude Code plugin whose skill drives the tool
pidba/              a Pi terminal agent that drives the tool with no shell of its own
docs/               labels-review.xlsx, every label rule laid out for review
```

## How an investigation narrows

1. **Labels.** Every metric of every node is read against a rule (`mongo-log-parser/internal/label/rules`)
   and described by its level, its shape and when it changed. A burst a minute mean would hide — a
   ticket pool empty for ten seconds — is counted in seconds. The report lays the labels out as
   worth opening, worth noticing, and ordinary.
2. **One label opened.** The exact seconds it was past its mark, the labels its rule pairs with read
   over those same seconds, the labels that moved with it, and what that node's log said then.
3. **The seconds and the lines.** The window second by second, then the log lines themselves.

The tool labels and does not diagnose; the cause is read from the labels beside the logs.

## Use it

Build the parser (Go 1.26 or later):

```bash
mongo-log-parser/scripts/build.sh          # bin/mongo-log-parser, stamped with its version
B=mongo-log-parser/bin/mongo-log-parser
$B coverage /path/to/bundle                # what the bundle holds, and its blind spots
$B /path/to/bundle                         # the report: what was running, what the labels saw, …
$B label /path/to/bundle tickets.read.empty --source rs0-1
$B label /path/to/bundle tickets.read.empty --source rs0-1 --at 10:00:00 --span 2m
$B labels                                  # the rule book
```

**From Claude Code**, add this repository as a marketplace and install the plugin; its skill walks
the same funnel and expects `mongo-log-parser` on `PATH` (or built as above):

```text
/plugin marketplace add ynsuy/pidba
/plugin install mongo-bundle@psb-mongo-analyzer
```

**From Pi**, `pidba/scripts/pidba` starts an agent whose only tools are the parser's commands; see
`pidba/README.md`.

## Develop

```bash
(cd mongo-log-parser && go vet ./... && go test ./...)
(cd pidba && npm ci && npm run verify)
(cd mongo-log-parser && DBA_FTDC_DIR=/path/to/diagnostic.data go test ./internal/label -run TestRulesReadPathsTheServerWrites)
python3 mongo-log-parser/scripts/labels-review.py   # regenerate docs/labels-review.xlsx (needs openpyxl)
```

The last test checks that every rule reads FTDC paths a real capture carries; run it against a
capture of each server version a rule changes for. The parser is versioned by tags named
`mongo-log-parser/vX.Y.Z`; pidba by its `package.json`; the plugin by `plugin/.claude-plugin/plugin.json`.
