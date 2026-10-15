package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/ynsuy/mongo-parser/internal/store"
)

// coverage answers "what is in this bundle, and what is not". Every later answer depends on it: a
// node that logged nothing for an hour, a log that stops before the incident, a day whose file
// arrived in pieces with the first one missing — any of these, unsaid, reads as "nothing happened".

type coverageData struct {
	Ingested   string       `json:"ingested_at"`
	First      string       `json:"first_log_line,omitempty"`
	Last       string       `json:"last_log_line,omitempty"`
	Inputs     []inputCount `json:"inputs"`
	Nodes      []nodeCover  `json:"nodes"`
	BlindSpots []blindSpot  `json:"blind_spots"`
	Unread     []inputCount `json:"not_read"`
	Point      []inputCount `json:"point_in_time_outputs"`
	Copies     int          `json:"copies_read_once"`
	// System is how many events of each kind the operating system's logs held.
	System map[string]int `json:"os_events,omitempty"`
}

type inputCount struct {
	Kind     string   `json:"kind"`
	Files    int      `json:"files"`
	Bytes    int64    `json:"bytes"`
	Examples []string `json:"examples,omitempty"`
}

type nodeCover struct {
	ID       int64          `json:"id"`
	Label    string         `json:"label"`
	Name     string         `json:"name"`
	ReplSet  string         `json:"replica_set,omitempty"`
	Role     string         `json:"role,omitempty"`
	Version  string         `json:"version,omitempty"`
	Evidence string         `json:"identified_by"`
	Aliases  []string       `json:"aliases,omitempty"`
	LogFirst string         `json:"log_first,omitempty"`
	LogLast  string         `json:"log_last,omitempty"`
	Lines    int            `json:"lines"`
	Streams  int            `json:"log_files"`
	Starts   int            `json:"startups_logged"`
	FTDC     []ftdcCover    `json:"ftdc,omitempty"`
	System   map[string]int `json:"os_events,omitempty"`
	logFirst int64
	logLast  int64
}

type ftdcCover struct {
	First  string `json:"first"`
	Last   string `json:"last"`
	Chunks int    `json:"chunks"`
	Files  int    `json:"files"`
	Origin string `json:"origin"`
	Note   string `json:"note,omitempty"`
	first  int64
	last   int64
}

type blindSpot struct {
	Node string `json:"node,omitempty"`
	What string `json:"what"`
}

func runCoverage(root string, args []string) error {
	fs, c := newFlags("coverage")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	st, err := openStore(root, c.refresh)
	if err != nil {
		return err
	}
	defer st.Close()
	data, err := buildCoverage(st)
	if err != nil {
		return err
	}
	return emit(os.Stdout, c.json, "coverage", root, data, renderCoverage(root, data))
}

var pointInTime = map[string]bool{"shell_output": true, "collector": true, "config": true}
var readKinds = map[string]bool{"log_json": true, "log_csv": true, "ftdc": true, "os_log": true}

func buildCoverage(st *store.Store) (*coverageData, error) {
	db := st.DB
	data := &coverageData{Ingested: st.Meta("ingested_at")}

	var first, last sql.NullInt64
	_ = db.QueryRow(`SELECT min(first_ts), max(last_ts) FROM block`).Scan(&first, &last)
	if first.Valid {
		data.First, data.Last = fmtTS(first.Int64), fmtTS(last.Int64)
	}

	// What kinds of files there were.
	rows, err := db.Query(`SELECT kind, count(*), sum(size), group_concat(path, char(31)) FROM source GROUP BY kind ORDER BY count(*) DESC`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ic inputCount
		var paths string
		if err := rows.Scan(&ic.Kind, &ic.Files, &ic.Bytes, &paths); err != nil {
			rows.Close()
			return nil, err
		}
		all := strings.Split(paths, "\x1f")
		sort.Strings(all)
		ic.Examples = all[:min(3, len(all))]
		base := strings.SplitN(ic.Kind, "/", 2)[0]
		switch {
		case readKinds[base]:
			ic.Examples = nil
			data.Inputs = append(data.Inputs, ic)
		case pointInTime[base]:
			data.Point = append(data.Point, ic)
		default:
			data.Unread = append(data.Unread, ic)
		}
	}
	rows.Close()

	// Nodes and what each one's logs and FTDC cover.
	nodes := map[int64]*nodeCover{}
	var order []int64
	rows, err = db.Query(`SELECT id, label, name, replset, role, version, evidence, aliases FROM node ORDER BY replset, label`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		n := &nodeCover{}
		var aliases string
		if err := rows.Scan(&n.ID, &n.Label, &n.Name, &n.ReplSet, &n.Role, &n.Version, &n.Evidence, &aliases); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal([]byte(aliases), &n.Aliases)
		nodes[n.ID] = n
		order = append(order, n.ID)
	}
	rows.Close()
	rows, err = db.Query(`SELECT node, min(first_ts), max(last_ts), sum(lines), count(*),
		sum(coalesce(json_extract(identity, '$.starts'), 0)) FROM stream WHERE lines > 0 GROUP BY node`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, f, l int64
		var lines, streams, starts int
		if err := rows.Scan(&id, &f, &l, &lines, &streams, &starts); err != nil {
			rows.Close()
			return nil, err
		}
		if n := nodes[id]; n != nil {
			n.logFirst, n.logLast, n.Lines, n.Streams, n.Starts = f, l, lines, streams, starts
			n.LogFirst, n.LogLast = fmtTS(f), fmtTS(l)
		}
	}
	rows.Close()
	rows, err = db.Query(`SELECT node, coalesce(first_ts, 0), coalesce(last_ts, 0), chunks, files, origin, note FROM ftdc ORDER BY first_ts`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var f ftdcCover
		if err := rows.Scan(&id, &f.first, &f.last, &f.Chunks, &f.Files, &f.Origin, &f.Note); err != nil {
			rows.Close()
			return nil, err
		}
		f.First, f.Last = fmtTS(f.first), fmtTS(f.last)
		if n := nodes[id]; n != nil {
			n.FTDC = append(n.FTDC, f)
		}
	}
	rows.Close()
	rows, err = db.Query(`SELECT coalesce(node, 0), kind, count(*) FROM os_event GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var kind string
		var count int
		if err := rows.Scan(&id, &kind, &count); err != nil {
			rows.Close()
			return nil, err
		}
		if data.System == nil {
			data.System = map[string]int{}
		}
		data.System[kind] += count
		if n := nodes[id]; n != nil {
			if n.System == nil {
				n.System = map[string]int{}
			}
			n.System[kind] += count
		}
	}
	rows.Close()
	for _, id := range order {
		data.Nodes = append(data.Nodes, *nodes[id])
	}

	_ = db.QueryRow(`SELECT count(*) FROM source WHERE note LIKE 'an exact copy of %'`).Scan(&data.Copies)
	data.BlindSpots = blindSpots(st, data, first.Int64, last.Int64)
	return data, nil
}

// blindSpots lists everything that limits what can be concluded, most consequential first.
func blindSpots(st *store.Store, data *coverageData, first, last int64) []blindSpot {
	db := st.DB
	var out []blindSpot
	add := func(node, format string, args ...any) {
		out = append(out, blindSpot{Node: node, What: fmt.Sprintf(format, args...)})
	}
	var lateStarts, earlyEnds []edge
	// Several nodes with the same limitation are one line naming them all.
	grouped := map[string][]string{}
	var groupOrder []string
	addEach := func(node, what string) {
		if _, ok := grouped[what]; !ok {
			groupOrder = append(groupOrder, what)
		}
		grouped[what] = append(grouped[what], node)
	}

	logs, ftdcs := 0, 0
	for _, n := range data.Nodes {
		if n.Lines > 0 {
			logs++
		}
		if len(n.FTDC) > 0 {
			ftdcs++
		}
	}
	if logs == 0 && ftdcs == 0 {
		if len(data.Point) > 0 {
			add("", "no MongoDB logs and no FTDC: only point-in-time shell outputs, which show the state at the moment they were taken and nothing about how it got there")
		} else {
			add("", "no MongoDB logs and no FTDC in this bundle")
		}
	} else if logs == 0 {
		add("", "no MongoDB logs: errors, slow operations, elections and connection events cannot be seen; only FTDC metrics")
	} else if ftdcs == 0 {
		add("", "no FTDC: there are no metrics (CPU, memory, cache, disk, queues, latency), so whether a slowdown came from resources or from the workload has to be inferred from the logs alone")
	}
	for _, u := range data.Unread {
		if strings.HasPrefix(u.Kind, "foreign") {
			add("", "%d files of another database (%s …) — this bundle may be for a different product", u.Files, u.Examples[0])
		}
		if strings.HasPrefix(u.Kind, "log_legacy_text") {
			add("", "%d MongoDB 4.2-or-older text logs are not read: %s", u.Files, strings.Join(u.Examples, ", "))
		}
	}

	// The logs and the metrics cover different stretches: say how much of the FTDC window has no
	// log line from anybody, because that is where log-based answers do not exist.
	var ftdcFirst, ftdcLast sql.NullInt64
	_ = db.QueryRow(`SELECT min(first_ts), max(last_ts) FROM ftdc WHERE chunks > 0`).Scan(&ftdcFirst, &ftdcLast)
	if logs > 0 && ftdcFirst.Valid {
		ftdcSpan, logSpan := ftdcLast.Int64-ftdcFirst.Int64, last-first
		if ftdcSpan > 0 && logSpan < ftdcSpan/2 {
			add("", "the logs cover %s (%s) of the %s the FTDC covers (%s): outside that stretch only metrics exist, no log line from any node",
				duration(logSpan), span(first, last), duration(ftdcSpan), span(ftdcFirst.Int64, ftdcLast.Int64))
		}
	}

	// Files that arrived broken. Copies are not a limitation and are only counted.
	rows, err := db.Query(`SELECT path, note, kind FROM source WHERE note != '' ORDER BY path`)
	if err == nil {
		seen := map[string]bool{}
		for rows.Next() {
			var path, note, kind string
			if rows.Scan(&path, &note, &kind) != nil {
				continue
			}
			var parts []string
			for _, part := range strings.Split(note, "; ") {
				if strings.HasPrefix(part, "an exact copy of") || strings.HasPrefix(part, "read once") || strings.HasPrefix(part, "kept once") {
					continue
				}
				parts = append(parts, part)
			}
			if len(parts) == 0 {
				continue
			}
			text := strings.Join(parts, "; ")
			if kind == "log_csv" {
				if seen[text] {
					continue
				}
				seen[text] = true
				add("", "log export: %s (the export's own ingest time can be seconds after the server wrote the line)", text)
				continue
			}
			add("", "%s: %s", path, text)
		}
		rows.Close()
	}

	for _, n := range data.Nodes {
		if strings.Contains(n.Evidence, "file name only") {
			addEach(n.Label, "no line in its logs says which process wrote them, so it is named after its file and could be the same node as another entry here")
		}
		if strings.Contains(n.Evidence, "ambiguous:") {
			addEach(n.Label, "its short name is shared by several nodes here and nothing in its files says which one it is")
		}
		if n.Lines > 0 && len(n.FTDC) == 0 && ftdcs > 0 {
			addEach(n.Label, "logs but no FTDC")
		}
		if n.Lines == 0 && len(n.FTDC) > 0 && logs > 0 {
			addEach(n.Label, "FTDC but no logs: its errors, slow operations and state changes are not in this bundle")
		}
		for _, f := range n.FTDC {
			if f.Note != "" {
				add(n.Label, "FTDC: %s", f.Note)
			}
		}
		if n.Lines > 0 && first > 0 {
			const slack = 30 * 60 * 1000
			if n.logFirst-first > slack {
				lateStarts = append(lateStarts, edge{n.Label, n.logFirst})
			}
			if last-n.logLast > slack {
				earlyEnds = append(earlyEnds, edge{n.Label, n.logLast})
			}
		}
		for _, f := range n.FTDC {
			if n.Lines > 0 && (f.first > n.logLast || f.last < n.logFirst) {
				add(n.Label, "its FTDC (%s) and its logs (%s) do not overlap at all", span(f.first, f.last), span(n.logFirst, n.logLast))
			}
		}
	}

	for _, what := range groupOrder {
		add(strings.Join(grouped[what], ", "), "%s", what)
	}
	for _, g := range groupEdges(lateStarts) {
		add(strings.Join(g.labels, ", "), "logs start at %s, %s after the bundle's first line (%s): before that, only the other nodes' logs exist",
			clockRange(g.from, g.to), duration(g.from-first), fmtTS(first))
	}
	for _, g := range groupEdges(earlyEnds) {
		add(strings.Join(g.labels, ", "), "logs end at %s, %s before the bundle's last line (%s): what happened to them after that is not in this bundle",
			clockRange(g.from, g.to), duration(last-g.to), fmtTS(last))
	}

	// Silences, read through the lines on either side of them: a gap that a fatal error opens and a
	// restart closes is a node that was down, which no count of lines would say.
	type silenceRow struct {
		label                  string
		node, from, to, others int64
		before, after          store.Ref
	}
	var silences []silenceRow
	rows, err = db.Query(`SELECT n.label, s.node, s.from_ts, s.to_ts, s.others, s.before_block, s.before_slot, s.after_block, s.after_slot
		FROM silence s JOIN node n ON n.id = s.node ORDER BY s.to_ts - s.from_ts DESC`)
	if err == nil {
		for rows.Next() {
			var r silenceRow
			if rows.Scan(&r.label, &r.node, &r.from, &r.to, &r.others, &r.before.Block, &r.before.Slot, &r.after.Block, &r.after.Slot) == nil {
				silences = append(silences, r)
			}
		}
		rows.Close()
	}
	var brokenLogs []string
	if rows, err := db.Query(`SELECT path FROM source WHERE note LIKE '%pieces missing%' OR (kind = 'other' AND note LIKE '%unreadable%')`); err == nil {
		for rows.Next() {
			var path string
			if rows.Scan(&path) == nil {
				brokenLogs = append(brokenLogs, path)
			}
		}
		rows.Close()
	}
	// Classify each silence, then fold silences of the same kind that several nodes went through
	// at the same time into one line: several nodes stopped together is one event, the deployment
	// being switched off, and a line per node says it again and again while hiding that it was one.
	type classified struct {
		label    string
		from, to int64
		kind     string // what the bracketing lines say, for grouping
		how      string // the sentence for one node
		together string // the sentence for several
	}
	var found []classified
	for _, r := range silences {
		var beforeSev, beforeMsg, beforeAttr, afterMsg string
		if line, err := st.Line(r.before); err == nil {
			beforeSev, beforeMsg, beforeAttr = line.Sev, line.Msg, string(line.Attr())
		}
		if line, err := st.Line(r.after); err == nil {
			afterMsg = line.Msg
		}
		if beforeSev == "F" || beforeSev == "E" {
			// The last line of a crash is usually the generic abort; the cause is a line or two
			// earlier in the same second.
			if cause := errorNear(st, r.node, r.from-2000, r.from); cause != nil {
				beforeMsg, beforeAttr = cause.Msg, string(cause.Attr())
			}
		}
		restarted := afterMsg == "***** SERVER RESTARTED *****" || afterMsg == "MongoDB starting" || afterMsg == "Build Info"
		c := classified{label: r.label, from: r.from, to: r.to}
		switch {
		case restarted && beforeSev == "F":
			c.kind = "fatal"
			c.how = fmt.Sprintf("it opens with a fatal error (%s) and closes with a restart: the process was down", clip(fatalSummary(beforeMsg, beforeAttr), 220))
			c.together = "each opens with a fatal error and closes with a restart: they were down"
		case restarted && cleanShutdown(beforeMsg):
			c.kind = "clean"
			c.how = "it opens with a clean shutdown and closes with a restart: the process was stopped"
			c.together = "each opens with a clean shutdown and closes with a restart: they were stopped together"
		case restarted && announcesShutdown.MatchString(beforeMsg):
			c.kind = "announced"
			c.how = fmt.Sprintf("it opens with the server announcing it would stop (%s) and closes with a restart: the process was down", clip(fatalSummary(beforeMsg, beforeAttr), 260))
			c.together = "each opens with the server announcing it would stop and closes with a restart: they were down"
		case restarted:
			c.kind = "unlogged"
			c.how = fmt.Sprintf("it closes with a restart, but the last line before it is not a shutdown (%s): the process stopped without logging it — crash, kill -9, OOM kill or host loss — or the log of that stretch is missing", clip(fatalSummary(beforeMsg, beforeAttr), 220))
			c.together = "each closes with a restart after a last line that is not a shutdown: they stopped without logging it — host loss, kill -9, OOM kill — or those stretches of log are missing"
		case r.others > 0:
			c.kind = "alone"
			c.how = fmt.Sprintf("the other nodes wrote about %s lines meanwhile: hung, or its log for that stretch is missing", thousands(int(r.others)))
			c.together = "the other nodes kept logging meanwhile: hung, or those stretches of log are missing"
		case len(brokenLogs) > 0:
			c.kind = "broken"
			c.how = fmt.Sprintf("a log file of this bundle could not be read (%s), and this stretch may be what it held", strings.Join(brokenLogs, ", "))
			c.together = c.how
		case len(data.Nodes) > 1:
			c.kind = "quiet"
			c.how = "no other node logged either"
			c.together = "nobody logged"
		default:
			c.kind = "quiet"
			c.how = "and it is the only node with logs here, so a quiet spell and a missing stretch look the same"
			c.together = c.how
		}
		// Short quiet spells nobody else was logging through are not worth a line.
		if !restarted && r.others == 0 && r.to-r.from < 3600000 {
			continue
		}
		found = append(found, c)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].from < found[j].from })
	const near = 10 * 60 * 1000
	used := make([]bool, len(found))
	type cluster struct {
		members  []classified
		from, to int64
	}
	var clusters []cluster
	for i := range found {
		if used[i] {
			continue
		}
		cl := cluster{members: []classified{found[i]}, from: found[i].from, to: found[i].to}
		used[i] = true
		for j := i + 1; j < len(found); j++ {
			if used[j] || found[j].kind != found[i].kind {
				continue
			}
			if abs64(found[j].from-found[i].from) <= near && abs64(found[j].to-found[i].to) <= near {
				cl.members = append(cl.members, found[j])
				used[j] = true
				cl.from, cl.to = min(cl.from, found[j].from), max(cl.to, found[j].to)
			}
		}
		clusters = append(clusters, cl)
	}
	sort.SliceStable(clusters, func(i, j int) bool {
		if len(clusters[i].members) != len(clusters[j].members) {
			return len(clusters[i].members) > len(clusters[j].members)
		}
		return clusters[i].to-clusters[i].from > clusters[j].to-clusters[j].from
	})
	withLogs := 0
	for _, n := range data.Nodes {
		if n.Lines > 0 {
			withLogs++
		}
	}
	for i, cl := range clusters {
		if i == 15 {
			add("", "%d more silences; mongo-parser lines --around <time> shows what surrounds any of them", len(clusters)-15)
			break
		}
		if len(cl.members) == 1 {
			m := cl.members[0]
			add(m.label, "no log line for %s (%s); %s", duration(m.to-m.from), span(m.from, m.to), m.how)
			continue
		}
		var labels []string
		for _, m := range cl.members {
			labels = append(labels, m.label)
		}
		who := strings.Join(labels, ", ")
		if len(cl.members) == withLogs {
			who = fmt.Sprintf("every node with logs (%d)", withLogs)
		}
		add(who, "no log line from %s to %s (about %s); %s", fmtTS(cl.from)[:16], fmtTS(cl.to)[:16], duration(cl.to-cl.from), cl.members[0].together)
	}

	// Lines that could not be read, and lines the server itself cut.
	rows, err = db.Query(`SELECT n.label, src.path, s.bad_lines, s.bad_example FROM stream s JOIN source src ON src.id = s.source
		LEFT JOIN node n ON n.id = s.node WHERE s.bad_lines > 0 ORDER BY s.bad_lines DESC LIMIT 10`)
	if err == nil {
		for rows.Next() {
			var name sql.NullString
			var path, example string
			var bad int
			if rows.Scan(&name, &path, &bad, &example) == nil {
				add(name.String, "%s: %s lines could not be read, e.g. %q", path, thousands(bad), clip(example, 120))
			}
		}
		rows.Close()
	}
	var unparsed int
	_ = db.QueryRow(`SELECT coalesce(sum(unparsed_attrs), 0) FROM stream`).Scan(&unparsed)
	if unparsed > 0 {
		add("", "%s exported lines kept their attributes as text only (not parseable); the lines themselves are complete", thousands(unparsed))
	}
	var truncated int
	_ = db.QueryRow(`SELECT coalesce(sum(truncated_lines), 0) FROM stream`).Scan(&truncated)
	if truncated > 0 {
		add("", "%s lines were cut short by the server itself (attribute size cap): values past the cut are missing from them", thousands(truncated))
	}
	// An OS log's lines carry no zone and no year; where the bundle did not say which, it is said here.
	if rows, err := db.Query(`SELECT clock, count(*) FROM os_event WHERE clock != '' GROUP BY clock ORDER BY count(*) DESC`); err == nil {
		for rows.Next() {
			var clock string
			var count int
			if rows.Scan(&clock, &count) == nil {
				add("", "operating system logs: %s: %s", plural(count, "event"), clock)
			}
		}
		rows.Close()
	}
	return out
}

// errorNear finds the first fatal or error line of a node in a window that names what went wrong.
func errorNear(st *store.Store, node, from, to int64) *store.Record {
	streams := nodeStreams(st, node)
	if len(streams) == 0 {
		return nil
	}
	var found *store.Record
	_ = st.Scan(store.Query{Streams: streams, From: from, To: to, SevMask: store.SevBit("F") | store.SevBit("E")}, func(r *store.Record) bool {
		var a map[string]any
		if json.Unmarshal(r.Attr(), &a) != nil {
			return true
		}
		if _, ok := a["error"]; ok {
			copy := *r
			found = &copy
			return false
		}
		if _, ok := a["reason"]; ok {
			copy := *r
			found = &copy
			return false
		}
		return true
	})
	return found
}

func nodeStreams(st *store.Store, node int64) []int64 {
	rows, err := st.DB.Query(`SELECT id FROM stream WHERE node = ?`, node)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

var announcesShutdown = regexp.MustCompile(`(?i)will now shut ?down|shutting down|terminat|aborting|will exit`)

func cleanShutdown(msg string) bool {
	lower := strings.ToLower(msg)
	switch lower {
	case "now exiting", "shutting down", "shutdown: going to close listening sockets", "received signal":
		return true
	}
	return strings.Contains(lower, "shutdown complete")
}

// fatalSummary is a line in one breath: its message with the placeholders filled in, and the
// error it carries.
func fatalSummary(msg, attr string) string {
	var a map[string]any
	if json.Unmarshal([]byte(attr), &a) == nil {
		msg = fillTemplate(msg, a)
		for _, key := range []string{"error", "reason", "status", "msg"} {
			if v, ok := a[key]; ok {
				text := fmt.Sprint(v)
				if m, isMap := v.(map[string]any); isMap {
					if blob, err := json.Marshal(m); err == nil {
						text = string(blob)
					}
				}
				return fmt.Sprintf("%s: %s", strings.Join(strings.Fields(msg), " "), text)
			}
		}
	}
	return strings.Join(strings.Fields(msg), " ")
}

var placeholder = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// fillTemplate puts attribute values into a message's {name} placeholders, the way the server's
// text log would have printed it.
func fillTemplate(msg string, attr map[string]any) string {
	return placeholder.ReplaceAllStringFunc(msg, func(m string) string {
		v, ok := attr[m[1:len(m)-1]]
		if !ok {
			return m
		}
		if text, isText := v.(string); isText {
			return text
		}
		blob, _ := json.Marshal(v)
		return string(blob)
	})
}

type edge struct {
	label string
	at    int64
}

type edgeGroup struct {
	labels   []string
	from, to int64
}

// groupEdges folds nodes whose logs start (or end) within ten minutes of each other into one group.
func groupEdges(edges []edge) []edgeGroup {
	sort.Slice(edges, func(i, j int) bool { return edges[i].at < edges[j].at })
	var out []edgeGroup
	for _, e := range edges {
		if n := len(out); n > 0 && e.at-out[n-1].from <= 10*60*1000 {
			out[n-1].labels = append(out[n-1].labels, e.label)
			out[n-1].to = e.at
			continue
		}
		out = append(out, edgeGroup{labels: []string{e.label}, from: e.at, to: e.at})
	}
	return out
}

func clockRange(from, to int64) string {
	if to-from < 60*1000 {
		return fmtTS(from)[:16]
	}
	return fmtTS(from)[:16] + "–" + fmtTS(to)[11:16]
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func renderCoverage(root string, d *coverageData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "bundle  %s\n", root)
	if d.First != "" {
		fmt.Fprintf(&b, "logs    %s → %s UTC\n", d.First, d.Last)
	}

	b.WriteString("\nread\n")
	if len(d.Inputs) == 0 {
		b.WriteString("  nothing: no MongoDB log or FTDC file was found\n")
	}
	for _, in := range d.Inputs {
		fmt.Fprintf(&b, "  %-18s %4d files  %s\n", in.Kind, in.Files, bytesText(in.Bytes))
	}
	if d.Copies > 0 {
		fmt.Fprintf(&b, "  (%d files in the bundle were exact copies of others and were read once)\n", d.Copies)
	}
	if len(d.System) > 0 {
		fmt.Fprintf(&b, "  (the operating system's logs are read for the events that end a process or stall a machine: %s)\n", eventCounts(d.System))
	}
	if len(d.Point) > 0 {
		b.WriteString("\npoint-in-time outputs (kept, not yet interpreted)\n")
		for _, in := range d.Point {
			fmt.Fprintf(&b, "  %-38s %4d\n", in.Kind, in.Files)
		}
	}

	if len(d.Nodes) > 0 {
		b.WriteString("\nnodes\n")
		rs := "\x00"
		for _, n := range d.Nodes {
			if n.ReplSet != rs {
				rs = n.ReplSet
				name := rs
				if name == "" {
					name = "(no replica set stated)"
					if strings.HasPrefix(n.Role, "mongos") {
						name = "mongos routers"
					}
				}
				fmt.Fprintf(&b, "  %s\n", name)
			}
			fmt.Fprintf(&b, "    %s", n.Label)
			var facts []string
			if n.Role != "" {
				facts = append(facts, n.Role)
			}
			if n.Version != "" {
				facts = append(facts, n.Version)
			}
			if len(facts) > 0 {
				fmt.Fprintf(&b, "  [%s]", strings.Join(facts, ", "))
			}
			b.WriteString("\n")
			if n.Lines > 0 {
				fmt.Fprintf(&b, "      logs  %s   %s lines, %d file(s)", span(n.logFirst, n.logLast), thousands(n.Lines), n.Streams)
				if n.Starts > 0 {
					fmt.Fprintf(&b, ", %d startup(s) logged", n.Starts)
				}
				b.WriteString("\n")
			} else {
				b.WriteString("      logs  none\n")
			}
			if len(n.FTDC) == 0 {
				b.WriteString("      ftdc  none\n")
			}
			for _, f := range n.FTDC {
				fmt.Fprintf(&b, "      ftdc  %s   %d files\n", span(f.first, f.last), f.Files)
			}
			if len(n.System) > 0 {
				fmt.Fprintf(&b, "      os    %s\n", eventCounts(n.System))
			}
			if n.Label != n.Name {
				fmt.Fprintf(&b, "      name  %s\n", n.Name)
			}
			fmt.Fprintf(&b, "      by    %s\n", n.Evidence)
		}
	}

	b.WriteString("\nblind spots\n")
	if len(d.BlindSpots) == 0 {
		b.WriteString("  none found\n")
	}
	for _, s := range d.BlindSpots {
		if s.Node != "" {
			fmt.Fprintf(&b, "  - %s: %s\n", s.Node, s.What)
		} else {
			fmt.Fprintf(&b, "  - %s\n", s.What)
		}
	}
	if len(d.Unread) > 0 {
		b.WriteString("\nnot read\n")
		for _, u := range d.Unread {
			fmt.Fprintf(&b, "  %-18s %4d files  e.g. %s\n", u.Kind, u.Files, clip(u.Examples[0], 90))
		}
	}
	return b.String()
}

// eventCounts lists event kinds with their counts, most frequent first.
func eventCounts(counts map[string]int) string {
	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if counts[kinds[i]] != counts[kinds[j]] {
			return counts[kinds[i]] > counts[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	parts := make([]string, len(kinds))
	for i, kind := range kinds {
		parts[i] = fmt.Sprintf("%s %d", strings.ReplaceAll(kind, "_", " "), counts[kind])
	}
	return strings.Join(parts, ", ")
}

func bytesText(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
