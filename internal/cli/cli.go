// Package cli is the command line: one command per question a DBA asks of a bundle.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/ynsuy/mongo-parser/internal/guard"
	"github.com/ynsuy/mongo-parser/internal/ingest"
	"github.com/ynsuy/mongo-parser/internal/mcpserver"
	"github.com/ynsuy/mongo-parser/internal/store"
)

const usage = `usage: mongo-parser [command] <bundle path> [flags]

Reads a MongoDB support bundle — logs, FTDC, shell outputs, OS logs, in any mix of directories
and archives — once into a local store, then answers questions about it.

With no command, a path gets the overall report: what was running, what the labels saw — every
metric of every node, sorted into what is worth opening, what is worth noticing, and the ordinary
rest — how much was logged over time, the slow queries and where their time went, the replication
and sharding events, the catalogue changes, the conditions the server logged, and the log groups.

An investigation narrows from there: open a label to see exactly which seconds it was past its
mark, what the labels it pairs with did in those seconds, and what the node's log said; then read
the seconds themselves with --at, and the log lines with lines.

commands:
  report      the overall report (the default)
  coverage    what the bundle holds: nodes, what each one's logs and FTDC cover, and the blind
              spots — read this first, because a question about a window nobody logged has no answer
  deployment  what was running: versions, roles, cache sizes, restarts
  metrics     chart the standard board, or whatever you name after the path
  logs        group the log lines by what they say; --group <id> prints the lines of one group
  lines       the log lines themselves, verbatim, filtered by node, time, severity, component,
              log id, connection, message or attribute
  slow        rank slow query shapes by the time they cost
  ddl         index builds and catalogue changes, with their timing
  health      the conditions the server logged without raising a severity
  storage     how much data there is, and which collections hold it
  shell       what the bundle's shell printouts say: serverStatus, rs.status, hostInfo, cmdline
  ingest      read the bundle into the store now (every other command does it when it has to)
  prune       the stores this machine is keeping, and what to remove
  label       open one label up on a node: its stretches past the mark, second by second, beside
              the labels it pairs with and the node's log in the same window
  labels      the rule book: every label, what it is computed from, what it is read on, its bands
  version     which build this is, and when it was built
  mcp         serve these commands as MCP tools over stdio, for Claude Code, Codex or any MCP client
  hook        the PreToolUse hook that keeps an agent reading bundles through the parser

The path may be written anywhere among the arguments, so the order never matters:

  mongo-parser report /data/bundle          mongo-parser /data/bundle slow --top 5
  mongo-parser /data/bundle                 mongo-parser slow --path /data/bundle --failed

report takes --only to print just some of its sections, comma separated:

  mongo-parser report /data/bundle --only labels,deployment
  mongo-parser report /data/bundle --only ddl,replication

The sections are ` + "deployment, labels, activity, slow, replication, storage, ddl, signals and logs." + `

A label is opened by name, on one node or on every node that has it:

  mongo-parser label /data/bundle tickets.read.empty --source rs0-1
  mongo-parser label /data/bundle tickets.read.empty --source rs0-1 --at 10:00:00 --span 2m

A word after the path names what metrics to draw — a board, a panel, a metric, or part of a path:

  mongo-parser metrics /data/bundle tcmalloc        the heap and free-list panels
  mongo-parser metrics /data/bundle --metric 'document/*'   the whole subtree, quoted or the shell eats it
  mongo-parser metrics /data/bundle --list          every board, every panel and every metric

common flags:
  --json      machine-readable output
  --refresh   read the bundle again even if the store is current
  --path      the bundle, where a positional word would be ambiguous

windows: --from and --to, or --around with --window, or --last 2h counted back from the end of
what the bundle recorded. Times are UTC: 2026-01-15T10:00:00Z, "2026-01-15 10:00", "01-15 10:00",
or 10:00 when the bundle covers one day. --around 10:00 --window 20m is the 20 minutes centred
on 10:00.
`

// commands is the whole command table: the word a caller types, and what answers it.
var commands = map[string]func(string, []string) error{
	"report":     runReport,
	"coverage":   runCoverage,
	"deployment": runDeployment,
	"topology":   runDeployment,
	"metrics":    runMetrics,
	"logs":       runLogs,
	"lines":      runLines,
	"slow":       runSlow,
	"ddl":        runDDL,
	"catalogue":  runDDL,
	"health":     runHealth,
	"signals":    runHealth,
	"storage":    runStorage,
	"data":       runStorage,
	"shell":      runShell,
	"ingest":     runIngest,
	"prune":      runPrune,
	"labels":     runLabels,
	"label":      runLabel,
}

// pathless are the commands that are about this machine rather than about one bundle, and so take
// no path at all.
var pathless = map[string]bool{"prune": true, "labels": true}

// Run is the whole command line: it picks the command, finds the bundle among the arguments, and
// turns whatever comes back into an exit status.
func Run(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	// An agent's host starts these with no bundle: the bundle arrives with each request.
	switch argv[0] {
	case "mcp":
		if err := mcpserver.Run(Release()); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	case "hook":
		return guard.Run(os.Stdin, os.Stdout, os.Stderr)
	}
	for _, word := range argv {
		switch word {
		case "-h", "--help", "help":
			fmt.Print(usage)
			return 0
		// Asked what it is, before anything else: this has to answer without a path, because the
		// question is about the binary and not about a bundle.
		case "-v", "--version", "version":
			fmt.Println(versionLine())
			return 0
		}
	}
	command, root, args, err := split(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n\n%s", err, usage)
		return 2
	}
	run, known := commands[command]
	if !known {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", command, usage)
		return 2
	}
	if err := run(root, args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// split picks the command and the bundle out of the arguments, in whatever order they were typed.
//
// Requiring the path immediately after the command made `mongo-parser <path> slow` an error, which is
// the order half the people who use this reach for first — and the error it produced was a usage
// screen, not an answer. So the command is whichever word names one, and the bundle is whichever
// word is a path that exists. --path says so outright, for the rare case where a flag's value
// would also be a path on this machine.
func split(argv []string) (command, root string, args []string, err error) {
	rest := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		word := argv[i]
		if value, ok := strings.CutPrefix(word, "--path="); ok {
			root = value
			continue
		}
		if word == "--path" || word == "-path" {
			if i+1 >= len(argv) {
				return "", "", nil, fmt.Errorf("--path needs the bundle after it")
			}
			root, i = argv[i+1], i+1
			continue
		}
		if command == "" && commands[word] != nil {
			command = word
			continue
		}
		// A word that names something on this disk is the bundle. Tested rather than assumed from
		// its position: a flag's value is a plain word too, and "app.orders" is not a directory.
		if root == "" && !strings.HasPrefix(word, "-") {
			if _, statErr := os.Stat(word); statErr == nil {
				root = word
				continue
			}
		}
		rest = append(rest, word)
	}
	if root == "" && !pathless[command] {
		return "", "", nil, fmt.Errorf("no bundle in that: give the path to a support bundle, a directory or an archive")
	}
	// Pointing the tool at a path and asking nothing in particular asks for the overall report.
	if command == "" {
		command = "report"
	}
	if absolute, absErr := filepath.Abs(root); absErr == nil {
		root = absolute
	}
	if resolved, linkErr := filepath.EvalSymlinks(root); linkErr == nil {
		root = resolved
	}
	return command, root, rest, nil
}

// common are the flags every command takes.
type common struct {
	json    bool
	refresh bool
	width   int
}

func newFlags(name string) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	c := &common{}
	fs.BoolVar(&c.json, "json", false, "machine-readable output")
	// A command that is about this machine rather than about one bundle has no bundle to re-read,
	// and a flag that is accepted and does nothing is a small lie told on every --help.
	if !pathless[name] {
		fs.BoolVar(&c.refresh, "refresh", false, "read the bundle again even if the store is current")
	}
	fs.IntVar(&c.width, "width", 0, "output width; the terminal's own by default")
	return fs, c
}

// parse accepts flags before and after positional words, and returns the words.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var words []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return words, nil
		}
		words = append(words, args[0])
		args = args[1:]
	}
}

// openStore returns the bundle's store, reading the bundle first when there is none or it is out
// of date.
func openStore(root string, refresh bool) (*store.Store, error) {
	dir := store.Dir(root)
	print, err := ingest.Fingerprint(root)
	if err != nil {
		return nil, err
	}
	if !refresh {
		st, ok, err := store.Open(dir, print)
		if err != nil {
			return nil, err
		}
		if ok {
			store.Remember(dir, root)
			return st, nil
		}
	}
	bar := newProgress()
	st, err := ingest.Run(root, dir, bar.update)
	bar.done()
	if err != nil {
		return nil, fmt.Errorf("reading the bundle: %w", err)
	}
	st.Close()
	st, ok, err := store.Open(dir, print)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("the store at %s was not completed", dir)
	}
	store.Remember(dir, root)
	tidy(dir)
	return st, nil
}

// tidy is best effort: a cache that could not be cleared is no reason to fail the answer.
func tidy(keep string) {
	removed, _ := store.Tidy(keep)
	if len(removed) == 0 || !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	var freed int64
	for _, item := range removed {
		freed += item.Bytes
	}
	fmt.Fprintf(os.Stderr, "removed %d old stores from the cache (%s); mongo-parser prune lists the rest\n",
		len(removed), bytesText(freed))
}

func runIngest(root string, args []string) error {
	fs, c := newFlags("ingest")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	st, err := openStore(root, true)
	if err != nil {
		return err
	}
	defer st.Close()
	var lines, streams, nodes int
	_ = st.DB.QueryRow(`SELECT coalesce(sum(lines), 0) FROM block`).Scan(&lines)
	_ = st.DB.QueryRow(`SELECT count(*) FROM stream`).Scan(&streams)
	_ = st.DB.QueryRow(`SELECT count(*) FROM node`).Scan(&nodes)
	summary := map[string]any{"store": filepath.Dir(st.Path), "lines": lines, "streams": streams, "nodes": nodes,
		"seconds": st.Meta("ingest_seconds")}
	return emit(os.Stdout, c.json, "ingest", root, summary, fmt.Sprintf(
		"read %s log lines in %s streams from %d nodes in %ss\nstore: %s\nnext: mongo-parser coverage %s",
		thousands(lines), thousands(streams), nodes, st.Meta("ingest_seconds"), filepath.Dir(st.Path), quoteArg(root)))
}

// envelope is the shape of every --json answer: what was asked, of what, and the answer.
type envelope struct {
	Command string   `json:"command"`
	Bundle  string   `json:"bundle"`
	Data    any      `json:"data"`
	Notes   []string `json:"notes,omitempty"`
	// Render is the same answer drawn as text. A caller that has to show the answer to a person —
	// an agent relaying it, a ticket being written — would otherwise redraw the tables and charts
	// this tool already knows how to draw, and draw them differently every time.
	Render string `json:"render"`
}

func emit(w io.Writer, asJSON bool, command, root string, data any, text string, notes ...[]string) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		var combined []string
		for _, group := range notes {
			combined = append(combined, group...)
		}
		return enc.Encode(envelope{Notes: combined, Command: command, Bundle: root, Data: data, Render: strings.TrimRight(text, "\n")})
	}
	_, err := fmt.Fprintln(w, strings.TrimRight(text, "\n"))
	return err
}

// progress draws one line on stderr while a bundle is read, and only on a terminal.
type progress struct {
	tty     bool
	started time.Time
	last    time.Time
}

func newProgress() *progress {
	return &progress{tty: term.IsTerminal(int(os.Stderr.Fd())), started: time.Now()}
}

func (p *progress) update(done, total int, what string) {
	if !p.tty || time.Since(p.last) < 100*time.Millisecond && done < total {
		return
	}
	p.last = time.Now()
	if len(what) > 50 {
		what = "…" + what[len(what)-49:]
	}
	fmt.Fprintf(os.Stderr, "\r\033[Kreading bundle %d/%d  %s  (%s)", done, total, what, time.Since(p.started).Round(time.Second))
}

func (p *progress) done() {
	if p.tty {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + thousands(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func quoteArg(s string) string {
	if strings.ContainsAny(s, " '\"()") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

const stamp = "2006-01-02 15:04:05.000"

func fmtTS(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format(stamp)
}

// span writes a range compactly: the second time drops the date when it is the same day.
func span(from, to int64) string {
	if from == 0 {
		return "-"
	}
	a, b := time.UnixMilli(from).UTC(), time.UnixMilli(to).UTC()
	if a.Format("2006-01-02") == b.Format("2006-01-02") {
		return a.Format("2006-01-02 15:04") + " → " + b.Format("15:04")
	}
	return a.Format("2006-01-02 15:04") + " → " + b.Format("2006-01-02 15:04")
}

func duration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	case d >= time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return d.Round(time.Second).String()
}
