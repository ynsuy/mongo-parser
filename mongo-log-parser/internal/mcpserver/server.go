// Package mcpserver serves the parser's commands as MCP tools over stdio, so any agent that speaks
// MCP — Claude Code, Codex, an IDE — can investigate a bundle without a launcher of its own.
//
// Every tool runs this same binary as a child with --json and hands the model the structured
// answer, so a tool answers exactly what the command line answers.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = `These tools read an offline MongoDB support bundle — logs, log exports, FTDC
(diagnostic.data), shell printouts and configs — through mongo-log-parser. Pass the bundle's local
directory or archive as path on every call. Read the bundle only through these tools: do not open
its files with shell or file tools unless the user explicitly asks for a raw file. Start an
unexplained problem with mongo_report; mongo_coverage says what the bundle holds. A result too large
for one message arrives in pages: read every page with mongo_page before concluding anything.`

// Run serves until the client closes stdin or the process is told to stop. A parser still running
// then is killed with it: a client that went away leaves nobody to read the answer, and a report
// left running on a large bundle held cores and gigabytes for hours.
func Run(version string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	s := &server{binary: self, pageBytes: pageBytes(), results: map[string][]string{}, alive: ctx}
	srv := mcp.NewServer(&mcp.Implementation{Name: "mongo-log-parser", Version: version},
		&mcp.ServerOptions{Instructions: instructions})
	s.register(srv)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

type server struct {
	binary    string
	pageBytes int
	alive     context.Context // ends when the server does

	// The first command on a bundle builds its store; a second one started meanwhile would build it
	// again over the first.
	run sync.Mutex

	pages   sync.Mutex
	results map[string][]string
	next    int
}

// pageBytes keeps one result well under the clients' tool-output limits (Claude Code warns at
// 10k tokens and stops at 25k; JSON runs about three bytes a token).
func pageBytes() int {
	if n, err := strconv.Atoi(os.Getenv("MONGO_LOG_PARSER_MCP_PAGE_BYTES")); err == nil && n >= 4096 {
		return n
	}
	return 48 * 1024
}

type bundle struct {
	Path string `json:"path" jsonschema:"The support bundle: a local directory or archive, absolute or ~/..."`
}

type window struct {
	From   string `json:"from,omitempty" jsonschema:"Start of the window, UTC: 2026-01-15T10:00:00Z, \"2026-01-15 10:00\", \"01-15 10:00\", or 10:00 when the bundle covers one day"`
	To     string `json:"to,omitempty" jsonschema:"End of the window, same forms as from"`
	Around string `json:"around,omitempty" jsonschema:"A moment to centre the window on, same forms as from"`
	Window string `json:"window,omitempty" jsonschema:"Width of the around window, e.g. 20m or 2h. 30m by default"`
	Last   string `json:"last,omitempty" jsonschema:"A window this long ending at the last thing the bundle recorded, e.g. 2h"`
}

func (w window) args() []string {
	return flags("from", w.From, "to", w.To, "around", w.Around, "window", w.Window, "last", w.Last)
}

// flags turns name, value pairs into --name value, leaving out the empty ones.
func flags(pairs ...any) []string {
	var out []string
	for i := 0; i+1 < len(pairs); i += 2 {
		name := pairs[i].(string)
		switch value := pairs[i+1].(type) {
		case string:
			if value != "" {
				out = append(out, "--"+name, value)
			}
		case int:
			if value != 0 {
				out = append(out, "--"+name, strconv.Itoa(value))
			}
		case bool:
			if value {
				out = append(out, "--"+name)
			}
		case []string:
			for _, item := range value {
				out = append(out, "--"+name, item)
			}
		}
	}
	return out
}

type reportIn struct {
	bundle
	Only []string `json:"only,omitempty" jsonschema:"Sections to print: deployment, labels, activity, slow, replication, storage, ddl, signals, logs. Every section by default."`
	window
}

type labelIn struct {
	bundle
	Label    string `json:"label" jsonschema:"The label to open, as the report printed it, e.g. tickets.read.empty"`
	Source   string `json:"source,omitempty" jsonschema:"The node to open it on, as the report printed it. Every node that has the label by default."`
	Instance string `json:"instance,omitempty" jsonschema:"The disk or replica set member, for a label that has them (the word after the label name in the report)"`
	At       string `json:"at,omitempty" jsonschema:"A moment to read second by second, same forms as from; take it from a stretch or window the label printed"`
	Span     string `json:"span,omitempty" jsonschema:"How much around at to print second by second, e.g. 2m. 4m by default, at most 15m"`
	Chart    bool   `json:"chart,omitempty" jsonschema:"Return signal_series for charting the label's derived signal in its signal unit"`
	Bin      int    `json:"bin,omitempty" jsonschema:"Signal chart bin size in seconds; automatically chosen by default"`
	window
}

type metricsIn struct {
	bundle
	Sources []string `json:"sources,omitempty" jsonschema:"Node names from mongo_coverage; several to compare nodes, or [\"all\"] for every stream. Every stream by default."`
	Metrics []string `json:"metrics,omitempty" jsonschema:"Metric paths, e.g. serverStatus/globalLock/currentQueue/total. * matches any run of characters and ? exactly one, so a subtree can be asked for whole: flowControl/*, wiredTiger/cache/*. FTDC holds numbers only: process type, host, version and other text are in mongo_deployment. Every column FTDC holds is reachable, not only the board's: the kernel's TCP counters are systemMetrics/netstat/Tcp:* and TcpExt:*"`
	Top     int      `json:"top,omitempty" jsonschema:"How many metrics of the standard board to draw when none are named, 1 to 40"`
	Height  int      `json:"height,omitempty" jsonschema:"Chart rows per metric, 2 to 20"`
	Bin     int      `json:"bin,omitempty" jsonschema:"Bin size in seconds; chosen from the window by default"`
	window
}

type logsIn struct {
	bundle
	Top      int    `json:"top,omitempty" jsonschema:"How many groups to show, 1 to 100"`
	Category string `json:"category,omitempty" jsonschema:"slow_query, election, initial_sync, recovery, replication, sharding, ddl, storage, network, authentication, query or other"`
	Severity string `json:"severity,omitempty" jsonschema:"Keep this severity and worse: F, E, W or I"`
	Source   string `json:"source,omitempty" jsonschema:"Node name from mongo_coverage: only the groups seen on that node"`
	Contains string `json:"contains,omitempty" jsonschema:"Only the groups whose message contains this text"`
	Group    string `json:"group,omitempty" jsonschema:"Group id from an earlier result: shows the lines themselves instead of the summary"`
	Records  int    `json:"records,omitempty" jsonschema:"How many lines to show for a group, 1 to 50"`
	window
}

type slowIn struct {
	bundle
	Top       int    `json:"top,omitempty" jsonschema:"How many shapes to show, 1 to 100"`
	Plan      string `json:"plan,omitempty" jsonschema:"COLLSCAN, IXSCAN, IDHACK, COUNT or EOF"`
	NS        string `json:"ns,omitempty" jsonschema:"Namespace, e.g. shop.orders"`
	Op        string `json:"op,omitempty" jsonschema:"Only these operations: a command name (find, aggregate, insert, update, delete, getMore, findAndModify), a comma-separated list of them, or a group: writes, reads, ddl"`
	QueryHash string `json:"query_hash,omitempty" jsonschema:"Exact queryHash or planCacheShapeHash from the log"`
	WaitedOn  string `json:"waited_on,omitempty" jsonschema:"Keep only the shapes whose biggest wait is this: flow control, write concern, lock acquire, ticket queue, storage read or remote shards"`
	Examples  *int   `json:"examples,omitempty" jsonschema:"How many real logged executions of the shape to show, slowest first, with the command each one ran. Defaults to 3 once a filter is given; 0 leaves them out."`
	NoDetail  bool   `json:"no_detail,omitempty" jsonschema:"Leave out the drill-down a filter would otherwise add, keeping only the ranked table"`
	window
}

type linesIn struct {
	bundle
	Node   string   `json:"node,omitempty" jsonschema:"Node name or part of one; comma-separated for several"`
	Sev    string   `json:"sev,omitempty" jsonschema:"Severities, comma-separated: F, E, W, I, D1..D5. W+ means W and worse, E+ means E and worse"`
	Comp   string   `json:"comp,omitempty" jsonschema:"Components, comma-separated: NETWORK, REPL, COMMAND, STORAGE, CONTROL ..."`
	ID     string   `json:"id,omitempty" jsonschema:"Log ids, comma-separated, e.g. 51803 for a slow operation"`
	Ctx    string   `json:"ctx,omitempty" jsonschema:"Thread or connection, e.g. conn12345"`
	Msg    string   `json:"msg,omitempty" jsonschema:"Text in the message, case-insensitive"`
	Grep   string   `json:"grep,omitempty" jsonschema:"Text anywhere in the line, case-insensitive"`
	Attr   []string `json:"attr,omitempty" jsonschema:"Attribute tests: path=value, path~part, or path? for present"`
	Limit  int      `json:"limit,omitempty" jsonschema:"How many lines to show, 1 to 200"`
	Newest bool     `json:"newest,omitempty" jsonschema:"Newest first"`
	Count  bool     `json:"count,omitempty" jsonschema:"Only count the matching lines, by node"`
	window
}

type shellIn struct {
	bundle
	Name string `json:"name,omitempty" jsonschema:"Which printout to read, e.g. serverstatus, rs_status, host_info, getcmdlineopts"`
}

type ddlIn struct {
	bundle
	NS   string `json:"ns,omitempty" jsonschema:"Namespace, e.g. shop.orders"`
	Kind string `json:"kind,omitempty" jsonschema:"One kind only, e.g. \"index build\", \"drop collection\", \"drop index\", \"shard collection\""`
	Top  int    `json:"top,omitempty" jsonschema:"How many rows of each table to show, 1 to 100"`
	window
}

type healthIn struct {
	bundle
	Top int `json:"top,omitempty" jsonschema:"How many conditions to show, 1 to 100"`
	window
}

type deploymentIn struct {
	bundle
	window
}

type pageIn struct {
	Result string `json:"result" jsonschema:"The paged_result id from the first page, e.g. r3"`
	Page   int    `json:"page" jsonschema:"The page to read, from 2 to the page count"`
}

func (s *server) register(srv *mcp.Server) {
	tool(srv, s, "mongo_report", "Read the whole bundle in one pass and report what stands out: what was running (server version, role, storage engine, cache size, restarts and unclean shutdowns), what the labels saw — every machine and server metric of every node read against its rule's bands and shape, sorted into the labels worth opening, the ones worth noticing, and a count of the ordinary rest — log volume over time split by severity, the slow query shapes that cost the most time and where that time went, replication and sharding events per node, the catalogue changes (index builds, drops, renames), the conditions the server logged without raising a severity, and the log groups worth reviewing. This is the right first call for an unexplained problem: the labels worth opening are where an investigation starts, and mongo_label opens one. Use only to keep one section when you already know which one you want.",
		func(p reportIn) (string, []string) {
			return "report", append(flags("only", strings.Join(p.Only, ",")), p.window.args()...)
		})
	tool(srv, s, "mongo_label", "Open one label up on a node: the step after the report's labels section. It gives the rule behind the label (what it measures and how its bands read), the label's usual level, worst minute and worst sample, stretches of samples past its mark, longest first, with observed timestamps; elapsed durations require cadence and gap checks, and for the longest few windows: the labels the rule pairs with, read over those samples beside their usual level, and what that node's log said in the same window — slow operations and where their time went, logged conditions, elections and step downs, index builds, error and warning counts. Set at to read a window sample by sample, the label and its pairs side by side. Set chart to return signal_series: the label's derived sample values with units, min/avg/max bins and timestamps. Label names are the ones the report prints, e.g. tickets.read.empty, cache.dirty_above_20, disk.await.",
		func(p labelIn) (string, []string) {
			args := append([]string{p.Label}, flags("source", p.Source, "instance", p.Instance, "at", p.At,
				"span", p.Span, "bin", p.Bin, "chart", p.Chart)...)
			return "label", append(args, p.window.args()...)
		})
	tool(srv, s, "mongo_coverage", "What the bundle actually holds, and what it does not: every node found, what each one's logs and FTDC cover in time, which nodes are named by what evidence, and the blind spots — stretches where a node wrote no log line at all, each one labelled with what it opened and closed with, so a gap left by a crash reads differently from one left by a missing rotation. Call this first on an unfamiliar bundle. Every other answer is bounded by what this reports: a question about a window nobody logged has no answer, and a count that looks low is often a log that was not collected rather than an event that did not happen.",
		func(bundle) (string, []string) { return "coverage", nil })
	tool(srv, s, "mongo_metrics", "Chart FTDC metrics over a window. Name metrics to see exactly those, or omit them for the standard overview board (queues, tickets, latency, operations, cache, cpu, disk), the same board for every node and every window. Name several sources to compare nodes: each stream is read over the same window and identified by its source, and the rows of one metric stay together.",
		func(p metricsIn) (string, []string) {
			args := flags("source", p.Sources, "metric", p.Metrics, "top", p.Top, "height", p.Height, "bin", p.Bin)
			return "metrics", append(args, p.window.args()...)
		})
	tool(srv, s, "mongo_logs", "Group every structured log line in the bundle, rotations and gzip included, and rank the groups by how much they matter: severity first, then failure signals, then event kind, with severity and category totals and log volume over time. Narrow with a category, a minimum severity, a node or a substring, and pass a group id to read the lines themselves with their attributes.",
		func(p logsIn) (string, []string) {
			args := flags("top", p.Top, "category", p.Category, "severity", p.Severity, "source", p.Source,
				"contains", p.Contains, "group", p.Group, "records", p.Records)
			return "logs", append(args, p.window.args()...)
		})
	tool(srv, s, "mongo_slow", "Rank slow query shapes by summed logged elapsed duration, with count, average, p95 and maximum duration, documents and keys examined, documents examined per document returned, bytes read, and what each shape mostly waited for. Under the table, summed timing fields across observed slow operations: flow control, write concern, lock acquire, ticket queue, storage read, remote shards, cpu, and what the log did not account for. Timing fields may overlap or be absent and do not establish cause. Always report the observed count and time bounds; a single slow execution does not establish recurrence or total workload frequency. Naming a filter — ns, plan, query_hash or waited_on — also adds a drill-down for the costliest shape it kept: duration and execution series, the full distribution of every measure with min, average, p50, p95, p99, max and total, and the slowest real logged executions with the command document each one ran. Query hashes are the original logged values. Rows without a hash are namespace/operation/plan groups, not known query shapes.",
		func(p slowIn) (string, []string) {
			args := flags("top", p.Top, "plan", p.Plan, "ns", p.NS, "op", p.Op, "query-hash", p.QueryHash,
				"waited-on", p.WaitedOn, "no-detail", p.NoDetail)
			if p.Examples != nil {
				args = append(args, "--examples", strconv.Itoa(*p.Examples))
			}
			return "slow", append(args, p.window.args()...)
		})
	tool(srv, s, "mongo_lines", "The log lines themselves, exactly as the server wrote them, filtered by node, time, severity, component, log id, connection or thread, message text, or an attribute test. This is what every other number here is made of: use it to confirm a finding before acting on it, to read what surrounds a moment (around plus window), or to follow one connection through a failure. Prefer an attribute test over a text search when the field is known — attr: [\"ns=shop.orders\"], [\"durationMillis?\"], [\"errName~Stale\"] — and ask for count when the question is how many rather than which ones.",
		func(p linesIn) (string, []string) {
			args := flags("node", p.Node, "sev", p.Sev, "comp", p.Comp, "id", p.ID, "ctx", p.Ctx, "msg", p.Msg,
				"grep", p.Grep, "attr", p.Attr, "limit", p.Limit, "newest", p.Newest, "count", p.Count)
			return "lines", append(args, p.window.args()...)
		})
	tool(srv, s, "mongo_storage", "How much data there is and where it sits: the size of each mount the server's files are on, how full it is, and what the collector recorded for them, together with the index rebuilds that would have to re-read it. Mount utilization is an observation, not a diagnosed failure.",
		func(bundle) (string, []string) { return "storage", nil })
	tool(srv, s, "mongo_shell", "What the one-off shell outputs in the bundle say: serverStatus, rs.status(), hostInfo, getCmdLineOpts and the rest, as the collector captured them. These are a single moment, not a history — the moment the bundle was taken. Call with no name to list what the bundle holds.",
		func(p shellIn) (string, []string) {
			if p.Name == "" {
				return "shell", nil
			}
			return "shell", []string{p.Name}
		})
	tool(srv, s, "mongo_deployment", "What was actually running: per process the role (mongod, mongos, shard server or config server), the version it is on now and the ones it was on before, when it last started, storage engine, WiredTiger cache size, replica set, whether authorization was on, how many times it started inside the window, and whether it ever came up after an unclean shutdown; and the version each process came up on at each restart in order. Starts the log missed are read from FTDC's uptime and systemd; the machine's RAM and CPUs, and what changed at a restart (cache, memory, CPUs), from FTDC; runs that ended without a shutdown (the kernel's OOM killer, an abort, a failed start) and machine events (a full file table, tasks hung on I/O, disk errors, boots) from the operating system's logs when the bundle has them. Call this before reading any other figure: a queue depth means one thing on a 4 GB cache and another on 200 GB, and a counter that reset at a restart is not a counter.",
		func(p deploymentIn) (string, []string) { return "deployment", p.window.args() })
	tool(srv, s, "mongo_ddl", "Index builds and catalogue changes with their timing: every build followed from the line that started it to the line that ended it, with how long it ran and whether it completed, failed, was aborted or never logged a completion — and beside them the drops, creates, renames, collMods, shardCollections and featureCompatibilityVersion changes. Filter by namespace or by kind.",
		func(p ddlIn) (string, []string) {
			return "ddl", append(flags("ns", p.NS, "kind", p.Kind, "top", p.Top), p.window.args()...)
		})
	tool(srv, s, "mongo_health", "The conditions MongoDB logs at severity I and therefore never ranks by severity: authentication failures, write conflicts, lock timeouts, cursors reaped, operations killed, heartbeat failures, members reported unreachable, stale routing tables, jumbo chunks, connection churn, storage operations that took too long, clock skew. Each with a count, a rate per minute, when it started and stopped and how many nodes saw it. Counts and rates do not establish impact or cause; review_priority is a navigation aid, not logged severity. A condition here is a pattern in the message text, not a verdict: confirm it with mongo_logs or mongo_lines.",
		func(p healthIn) (string, []string) {
			return "health", append(flags("top", p.Top), p.window.args()...)
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "mongo_page", Annotations: readOnly, Description: "Read another page of a parser answer that was too big for one result. A paged answer starts with a line naming its result id and page count; it is one JSON document split at byte boundaries, so join the pages in order and read every page before concluding anything from it. Nothing in a paged answer is omitted."},
		func(_ context.Context, _ *mcp.CallToolRequest, p pageIn) (*mcp.CallToolResult, any, error) {
			s.pages.Lock()
			pages, ok := s.results[p.Result]
			s.pages.Unlock()
			if !ok {
				return nil, nil, fmt.Errorf("no paged result %s in this session; run the tool again", p.Result)
			}
			if p.Page < 1 || p.Page > len(pages) {
				return nil, nil, fmt.Errorf("result %s has pages 1 to %d", p.Result, len(pages))
			}
			return text(pageHeader(p.Result, p.Page, len(pages)) + "\n" + pages[p.Page-1]), nil, nil
		})
}

// readOnly marks every tool as reading a local bundle and changing nothing, which lets a host run
// them without asking each time.
var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(false)}

// bundled is every tool input: it carries the bundle path.
type bundled interface{ bundlePath() string }

func (b bundle) bundlePath() string { return b.Path }

// tool registers a parser command: args turns the tool's input into the command and its flags.
func tool[In bundled](srv *mcp.Server, s *server, name, description string, args func(In) (string, []string)) {
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: description, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, p In) (*mcp.CallToolResult, any, error) {
			root, err := resolve(p.bundlePath())
			if err != nil {
				return nil, nil, err
			}
			command, rest := args(p)
			out, err := s.parser(ctx, command, root, rest)
			if err != nil {
				return nil, nil, err
			}
			return text(s.paged(out)), nil, nil
		})
}

func resolve(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is required: the bundle's local directory or archive")
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, rest)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("bundle not found: %s", path)
	}
	return path, nil
}

// parser runs one command against the bundle and returns its JSON answer without the drawn text,
// which the model has no use for and which would double the size of every answer.
func (s *server) parser(ctx context.Context, command, root string, args []string) (string, error) {
	argv := append([]string{command, "--path", root, "--json", "--width", "120"}, args...)
	s.run.Lock()
	defer s.run.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.alive, cancel)()
	cmd := exec.CommandContext(ctx, s.binary, argv...)
	// A server killed outright runs no code at all; the child notices its parent is gone instead.
	cmd.Env = append(os.Environ(), "MONGO_LOG_PARSER_PARENT="+strconv.Itoa(os.Getpid()))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		return "", fmt.Errorf("parser failed: %s", reason)
	}
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil {
		return "", fmt.Errorf("parser returned invalid JSON for %s; no evidence was accepted", command)
	}
	delete(answer, "render")
	answer["requested_args"], _ = json.Marshal(args)
	out, err := json.Marshal(answer)
	return string(out), err
}

// paged returns text whole when it fits a page, or its first page with a header naming the rest.
func (s *server) paged(text string) string {
	pages := paginate(text, s.pageBytes)
	if len(pages) == 1 {
		return text
	}
	s.pages.Lock()
	s.next++
	id := fmt.Sprintf("r%d", s.next)
	s.results[id] = pages
	s.pages.Unlock()
	return pageHeader(id, 1, len(pages)) + "\n" + pages[0]
}

// paginate splits text into parts of at most size bytes, never inside a character. The parts
// joined are the text exactly.
func paginate(text string, size int) []string {
	if len(text) <= size {
		return []string{text}
	}
	var parts []string
	for len(text) > 0 {
		end := min(size, len(text))
		for end < len(text) && end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		parts = append(parts, text[:end])
		text = text[end:]
	}
	return parts
}

// pageHeader says the text after it is one part of one JSON document, so a first page is never
// taken for the whole answer.
func pageHeader(id string, page, pages int) string {
	rest := "This is the last page."
	if page < pages {
		rest = fmt.Sprintf("Read pages %d to %d with mongo_page (result %q) before drawing conclusions from it.", page+1, pages, id)
	}
	header, _ := json.Marshal(map[string]any{"paged_result": id, "page": page, "pages": pages,
		"note": fmt.Sprintf("The parser's structured evidence is one JSON document sent in %d pages; nothing is omitted. The text after this line is page %d, joined to the next page exactly where it stops. %s", pages, page, rest)})
	return string(header)
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}
