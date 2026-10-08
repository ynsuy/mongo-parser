package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mongo-log-parser/internal/store"
	"mongo-log-parser/internal/timeparse"
)

// lines shows log lines exactly as they were written. Every number any other command prints can be
// traced back to lines this command shows, which is what makes the numbers checkable.

type lineOut struct {
	TS        string          `json:"ts"`
	Node      string          `json:"node"`
	Source    string          `json:"source"`
	Line      int             `json:"line"`
	Sev       string          `json:"s"`
	Comp      string          `json:"c"`
	ID        int             `json:"id"`
	Ctx       string          `json:"ctx"`
	Msg       string          `json:"msg"`
	Attr      json.RawMessage `json:"attr,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	Raw       string          `json:"raw"`
}

type linesData struct {
	Matched int       `json:"matched"`
	Shown   int       `json:"shown"`
	Offset  int       `json:"offset"`
	Nodes   []string  `json:"nodes,omitempty"`
	From    string    `json:"from,omitempty"`
	To      string    `json:"to,omitempty"`
	Lines   []lineOut `json:"lines"`
	ByNode  []kv      `json:"by_node,omitempty"`
}

type kv struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

func runLines(root string, args []string) error {
	fs, c := newFlags("lines")
	node := fs.String("node", "", "node name, alias or part of one; comma-separated for several")
	from := fs.String("from", "", "start time (UTC)")
	to := fs.String("to", "", "end time (UTC)")
	around := fs.String("around", "", "a moment to centre the window on (UTC)")
	window := fs.String("window", "30m", "width of the --around window")
	last := fs.Duration("last", 0, "window ending at the last thing the bundle recorded, e.g. 2h")
	sev := fs.String("sev", "", "severities, comma-separated: F,E,W,I,D1..D5; W+ means W and worse")
	fs.StringVar(sev, "severity", "", "the same as --sev")
	comp := fs.String("comp", "", "components, comma-separated: NETWORK,REPL,COMMAND,STORAGE ...")
	ids := fs.String("id", "", "log ids, comma-separated, e.g. 51803 (slow operation)")
	ctx := fs.String("ctx", "", "thread or connection, e.g. conn12345")
	msg := fs.String("msg", "", "text in the message, case-insensitive")
	grep := fs.String("grep", "", "text anywhere in the line, case-insensitive")
	var attrs multi
	fs.Var(&attrs, "attr", "attribute test: path=value, path~part, or path? (present); repeatable, e.g. ns=shop.orders, errName~Stale")
	limit := fs.Int("limit", 50, "lines to show")
	offset := fs.Int("offset", 0, "skip this many matching lines first")
	newest := fs.Bool("newest", false, "newest first")
	format := fs.String("format", "raw", "raw (the line as written) or short (time, node, severity, message, attributes)")
	count := fs.Bool("count", false, "only count the matching lines, by node")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	st, err := openStore(root, c.refresh)
	if err != nil {
		return err
	}
	defer st.Close()

	f, err := newLineFilter(st, filterArgs{*node, *from, *to, *around, *window, *sev, *comp, *ids, *ctx, *msg, *grep, *last, attrs})
	if err != nil {
		return err
	}
	f.query.Newest = *newest
	data := &linesData{Offset: *offset, Nodes: f.names, From: f.from, To: f.to}
	labels, err := streamLabels(st)
	if err != nil {
		return err
	}
	sources, err := streamSources(st)
	if err != nil {
		return err
	}
	byNode := map[string]int{}
	err = st.Scan(f.query, func(r *store.Record) bool {
		if !f.match(r) {
			return true
		}
		data.Matched++
		byNode[labels[r.Stream]]++
		if !*count && data.Matched > *offset && len(data.Lines) < *limit {
			o := lineOut{TS: fmtTS(r.TS), Node: labels[r.Stream], Source: sources[r.Stream], Line: r.Line, Sev: r.Sev, Comp: r.Comp,
				ID: r.LogID, Ctx: r.Ctx, Msg: r.Msg, Truncated: r.Truncated, Raw: r.Raw}
			if r.HasSeparateAttr() {
				o.Attr = r.Attr()
			}
			data.Lines = append(data.Lines, o)
		}
		return true
	})
	if err != nil {
		return err
	}
	data.Shown = len(data.Lines)
	if *count {
		for k, v := range byNode {
			data.ByNode = append(data.ByNode, kv{k, v})
		}
		sort.Slice(data.ByNode, func(i, j int) bool { return data.ByNode[i].Count > data.ByNode[j].Count })
		var b strings.Builder
		fmt.Fprintf(&b, "%s matching lines\n", thousands(data.Matched))
		for _, k := range data.ByNode {
			fmt.Fprintf(&b, "  %8s  %s\n", thousands(k.Count), k.Key)
		}
		return emit(os.Stdout, c.json, "lines", root, data, b.String())
	}
	return emit(os.Stdout, c.json, "lines", root, data, renderLines(data, *format, *newest))
}

func streamLabels(st *store.Store) (map[int64]string, error) {
	rows, err := st.DB.Query(`SELECT s.id, coalesce(n.label, '?') FROM stream s LEFT JOIN node n ON n.id = s.node`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var label string
		if rows.Scan(&id, &label) == nil {
			out[id] = label
		}
	}
	return out, nil
}

func streamSources(st *store.Store) (map[int64]string, error) {
	rows, err := st.DB.Query(`SELECT s.id, src.path FROM stream s JOIN source src ON src.id = s.source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var path string
		if rows.Scan(&id, &path) == nil {
			out[id] = path
		}
	}
	return out, nil
}

type filterArgs struct {
	node, from, to, around, window, sev, comp, ids, ctx, msg, grep string
	last                                                           time.Duration
	attrs                                                          []string
}

// lineFilter is a query the block index can narrow, plus the tests only a line can answer.
type lineFilter struct {
	query    store.Query
	names    []string
	from, to string
	comps    map[string]bool
	ctx      string
	msg      string
	grep     string
	attrs    []attrTest
}

type attrTest struct {
	path  []string
	kind  byte
	value string
	num   *float64
}

var attrPath = regexp.MustCompile(`^[A-Za-z0-9_$.]+$`)

func newLineFilter(st *store.Store, f filterArgs) (*lineFilter, error) {
	lf := &lineFilter{ctx: f.ctx, msg: strings.ToLower(f.msg), grep: strings.ToLower(f.grep)}
	if f.node != "" {
		ids, names, err := resolveNodes(st, f.node)
		if err != nil {
			return nil, err
		}
		lf.names = names
		marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		params := make([]any, len(ids))
		for i, id := range ids {
			params[i] = id
		}
		rows, err := st.DB.Query(`SELECT id FROM stream WHERE node IN (`+marks+`)`, params...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				lf.query.Streams = append(lf.query.Streams, id)
			}
		}
		rows.Close()
		if len(lf.query.Streams) == 0 {
			lf.query.Streams = []int64{-1}
		}
	}
	if f.last > 0 {
		if f.from != "" || f.to != "" || f.around != "" {
			return nil, fmt.Errorf("--last cannot be combined with --from, --to or --around")
		}
		var last sql.NullInt64
		_ = st.DB.QueryRow(`SELECT max(last_ts) FROM block`).Scan(&last)
		if !last.Valid {
			return nil, fmt.Errorf("--last needs something recorded to count back from, and this bundle holds no log line")
		}
		end := time.UnixMilli(last.Int64).UTC()
		start := end.Add(-f.last)
		lf.query.From, lf.query.To = start.UnixMilli(), end.UnixMilli()
		lf.from, lf.to = start.Format(stamp), end.Format(stamp)
	}
	if f.from != "" || f.to != "" || f.around != "" {
		var first, last sql.NullInt64
		_ = st.DB.QueryRow(`SELECT min(first_ts), max(last_ts) FROM block`).Scan(&first, &last)
		window := ""
		if f.around != "" {
			window = f.window
		}
		start, end, err := timeparse.Window(f.from, f.to, f.around, window, time.UnixMilli(first.Int64).UTC(), time.UnixMilli(last.Int64).UTC())
		if err != nil {
			return nil, err
		}
		if !start.IsZero() {
			lf.query.From = start.UnixMilli()
			lf.from = start.Format(stamp)
		}
		if !end.IsZero() {
			lf.query.To = end.UnixMilli()
			lf.to = end.Format(stamp)
		}
	}
	for _, s := range splitList(strings.ToUpper(f.sev)) {
		switch s {
		case "W+":
			lf.query.SevMask |= store.SevBit("W") | store.SevBit("E") | store.SevBit("F")
		case "E+":
			lf.query.SevMask |= store.SevBit("E") | store.SevBit("F")
		default:
			lf.query.SevMask |= store.SevBit(s)
		}
	}
	if f.comp != "" {
		lf.comps = map[string]bool{}
		for _, p := range splitList(f.comp) {
			lf.comps[strings.ToUpper(p)] = true
		}
	}
	for _, p := range splitList(f.ids) {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("--id %q is not a number", p)
		}
		lf.query.IDs = append(lf.query.IDs, n)
	}
	for _, test := range f.attrs {
		op := strings.IndexAny(test, "=~?")
		if op <= 0 {
			return nil, fmt.Errorf("--attr %q: use path=value, path~part or path?", test)
		}
		path := test[:op]
		if !attrPath.MatchString(path) {
			return nil, fmt.Errorf("--attr %q: a path is letters, digits and dots", test)
		}
		t := attrTest{path: strings.Split(path, "."), kind: test[op], value: test[op+1:]}
		if n, err := strconv.ParseFloat(t.value, 64); err == nil {
			t.num = &n
		}
		if t.kind == '~' {
			t.value = strings.ToLower(t.value)
		}
		lf.attrs = append(lf.attrs, t)
	}
	return lf, nil
}

func (f *lineFilter) match(r *store.Record) bool {
	if f.comps != nil && !f.comps[strings.ToUpper(r.Comp)] {
		return false
	}
	if f.ctx != "" && r.Ctx != f.ctx {
		return false
	}
	if f.msg != "" && !strings.Contains(strings.ToLower(r.Msg), f.msg) {
		return false
	}
	if f.grep != "" && !strings.Contains(strings.ToLower(r.Raw), f.grep) {
		return false
	}
	if len(f.attrs) > 0 {
		var attr map[string]any
		if raw := r.Attr(); raw == nil || json.Unmarshal(raw, &attr) != nil {
			return false
		}
		for _, t := range f.attrs {
			v, ok := lookup(attr, t.path)
			switch t.kind {
			case '?':
				if !ok {
					return false
				}
			case '=':
				if !ok || !equalValue(v, t) {
					return false
				}
			case '~':
				if !ok || !strings.Contains(strings.ToLower(valueText(v)), t.value) {
					return false
				}
			}
		}
	}
	return true
}

func lookup(m map[string]any, path []string) (any, bool) {
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = obj[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func equalValue(v any, t attrTest) bool {
	if n, ok := v.(float64); ok && t.num != nil {
		return n == *t.num
	}
	return valueText(v) == t.value
}

func valueText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	blob, _ := json.Marshal(v)
	return string(blob)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// resolveNodes finds nodes by name, alias or part of one. An exact match wins; otherwise every node
// containing the text matches, so "rs0" can mean rs0-1 and rs0-2 together.
func resolveNodes(st *store.Store, query string) ([]int64, []string, error) {
	type node struct {
		id      int64
		label   string
		name    string
		aliases []string
	}
	rows, err := st.DB.Query(`SELECT id, label, name, aliases FROM node`)
	if err != nil {
		return nil, nil, err
	}
	var all []node
	for rows.Next() {
		var n node
		var aliases string
		if rows.Scan(&n.id, &n.label, &n.name, &aliases) == nil {
			_ = json.Unmarshal([]byte(aliases), &n.aliases)
			all = append(all, n)
		}
	}
	rows.Close()
	var ids []int64
	var names []string
	for _, want := range splitList(query) {
		want = strings.ToLower(want)
		var exact, partial []node
		for _, n := range all {
			candidates := append([]string{strings.ToLower(n.label), strings.ToLower(n.name)}, n.aliases...)
			if strconv.FormatInt(n.id, 10) == want {
				exact = append(exact, n)
				continue
			}
			for _, c := range candidates {
				if c == want {
					exact = append(exact, n)
					break
				}
				if strings.Contains(c, want) {
					partial = append(partial, n)
					break
				}
			}
		}
		matched := exact
		if len(matched) == 0 {
			matched = partial
		}
		if len(matched) == 0 {
			var known []string
			for _, n := range all {
				known = append(known, n.label)
			}
			return nil, nil, fmt.Errorf("no node matches %q; nodes are: %s", want, strings.Join(known, ", "))
		}
		for _, n := range matched {
			ids = append(ids, n.id)
			names = append(names, n.label)
		}
	}
	return ids, names, nil
}

func renderLines(d *linesData, format string, newest bool) string {
	var b strings.Builder
	for _, l := range d.Lines {
		switch format {
		case "short":
			attr := ""
			if l.Attr != nil {
				attr = string(l.Attr)
			} else if i := strings.Index(l.Raw, `"attr":`); i >= 0 {
				attr = strings.TrimSuffix(l.Raw[i+7:], "}")
			}
			msg := strings.Join(strings.Fields(l.Msg), " ")
			fmt.Fprintf(&b, "%s  %-28s %-2s %-10s %-14s %s  %s\n", l.TS, clip(l.Node, 28), l.Sev, l.Comp, clip(l.Ctx, 14), msg, clip(attr, 400))
		default:
			fmt.Fprintf(&b, "[%s %s:%d]\n%s\n", l.Node, l.Source, l.Line, l.Raw)
		}
	}
	direction := "oldest first"
	if newest {
		direction = "newest first"
	}
	if d.Matched == 0 {
		b.WriteString("no line matches\n")
	} else {
		fmt.Fprintf(&b, "\nshowing %s of %s matching lines (%s)", thousands(d.Shown), thousands(d.Matched), direction)
		if d.Offset > 0 {
			fmt.Fprintf(&b, ", from %s", thousands(d.Offset+1))
		}
		if rest := d.Matched - d.Offset - d.Shown; rest > 0 {
			fmt.Fprintf(&b, "; --offset %d for the next", d.Offset+d.Shown)
		}
		b.WriteString("\n")
	}
	return b.String()
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }
