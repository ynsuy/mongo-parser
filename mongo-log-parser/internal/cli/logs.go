package cli

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"mongo-log-parser/internal/analyze"
	"mongo-log-parser/internal/render"
	"mongo-log-parser/internal/store"
)

// runLogs groups the log lines of the whole bundle by what they say, which is how a log of
// millions of lines becomes a page. `lines` beside it prints the lines themselves, verbatim; this
// one answers "what is in here", and --group then prints the lines of one answer.
func runLogs(root string, args []string) error {
	flags, c := newFlags("logs")
	w := addWindowFlags(flags)
	top := flags.Int("top", 25, "how many groups to show")
	category := flags.String("category", "", "only this category: "+categoryList())
	severity := flags.String("sev", "", "only this severity or worse: F, E, W or I")
	flags.StringVar(severity, "severity", "", "the same as --sev")
	source := flags.String("source", "", "only the lines from this node")
	contains := flags.String("contains", "", "only the groups whose message contains this text")
	group := flags.String("group", "", "show the lines of one group instead of the summary")
	records := flags.Int("records", 10, "how many lines to show for a group")
	noChart := flags.Bool("no-chart", false, "table only, without the activity chart above it")
	color := flags.String("color", "auto", "auto, always or never")
	if err := parseOnly(flags, args); err != nil {
		return err
	}
	if err := bounded(bound{"top", *top, 1}, bound{"records", *records, 1}); err != nil {
		return err
	}
	started := time.Now()
	st, err := openStore(root, c.refresh)
	if err != nil {
		return err
	}
	defer st.Close()
	start, end, err := w.resolve(st, time.Time{})
	if err != nil {
		return err
	}
	q, err := readQuality(st)
	if err != nil {
		return err
	}
	if *group != "" {
		return showGroup(st, root, c, *group, *records, start, end, c.width, started)
	}

	// The chart is drawn from the whole log rather than from the filtered groups: "when was this
	// deployment busy" is not a question about the category being looked at, and a chart that moved
	// with every filter could not be read against the one drawn beside another filter.
	grouper, activity := analyze.NewGrouper(), analyze.NewActivity()
	if err := analyze.Run(st, windowQuery(start, end), grouper.Only(*category), activity); err != nil {
		return err
	}
	groups := make([]analyze.Group, 0, 64)
	for _, candidate := range grouper.Groups() {
		if !keepsSeverity(candidate.Severity, *severity) {
			continue
		}
		if *contains != "" && !strings.Contains(strings.ToLower(candidate.Message), strings.ToLower(*contains)) {
			continue
		}
		if *source != "" && !matchesSource(candidate.Sources, *source) {
			continue
		}
		groups = append(groups, candidate)
	}
	if len(groups) == 0 {
		// With the counts, because "nothing matched" and "nothing could be read" look identical
		// and are not: a directory of files the store could not parse has to say so.
		return emit(os.Stdout, c.json, "logs", root, map[string]any{
			"groups": groups, "quality": q, "total_groups": 0, "activity": activity,
		}, withVersion("no log lines matched\n\n"+storeNote(q)))
	}
	shownGroups := groups[:min(*top, len(groups))]

	text := func(read func(analyze.Group) string) []string {
		values := make([]string, len(shownGroups))
		for i, g := range shownGroups {
			values[i] = read(g)
		}
		return values
	}
	rendered := render.Table([]render.Column{
		// The group id never drops: it is the handle --group takes, and a table that has dropped it
		// can be read but not followed.
		{Title: "Group", Values: text(func(g analyze.Group) string { return g.ID })},
		{Title: "Sev", Values: text(func(g analyze.Group) string { return g.Severity })},
		{Title: "Component", Values: text(func(g analyze.Group) string { return g.Component }), Drop: 3},
		{Title: "ID", Align: render.AlignRight, Values: text(func(g analyze.Group) string { return strconv.Itoa(g.MessageID) }), Drop: 4},
		{Title: "Count", Align: render.AlignRight, Values: text(func(g analyze.Group) string { return strconv.Itoa(g.Count) })},
		{Title: "Category", Values: text(func(g analyze.Group) string { return string(g.Category) }), Drop: 2},
		{Title: "First", Values: text(func(g analyze.Group) string { return g.First.Format("01-02 15:04") }), Drop: 5},
		{Title: "Last", Values: text(func(g analyze.Group) string { return g.Last.Format("01-02 15:04") }), Drop: 1},
		{Title: "Message", Align: render.AlignLeft, Values: text(func(g analyze.Group) string { return g.Message }), Elastic: true},
	}, render.TableOptions{Width: outputWidth(c.width), Color: useColor(*color) && !c.json})
	if !*noChart {
		chartFrom, chartTo := slowWindow(start, end, q, nil)
		if chart := activityChart(activity, chartFrom, chartTo, 0, outputWidth(c.width), useColor(*color) && !c.json); chart != "" {
			rendered = chart + "\n\n" + rendered
		}
	}
	if line := severityLine(activity); line != "" {
		rendered += "\n" + line
	}

	footer := fmt.Sprintf("%s%s; the bundle holds %s in %s (%d unparsable) covering %s .. %s; answered in %s",
		askedWindow(start, end),
		plural(len(groups), "group"), plural(q.Parsed, "record"), plural(q.Files, "log stream"), q.Invalid,
		q.FirstAt.Format(time.RFC3339), q.LastAt.Format(time.RFC3339), time.Since(started).Round(time.Millisecond))
	return emit(os.Stdout, c.json, "logs", root, map[string]any{
		"groups": shownGroups, "quality": q, "total_groups": len(groups), "activity": activity,
	}, withVersion(rendered+"\n\n"+footer))
}

// storeNote is what the store managed to read, for the answers that have nothing else to report.
// "Nothing matched" and "nothing could be parsed" look identical on screen and are different
// problems: one is a filter, the other is a bundle this tool cannot read.
func storeNote(q quality) string {
	note := fmt.Sprintf("%s read from %s, %s parsed",
		plural(q.Lines, "line"), plural(q.Files, "log stream"), plural(q.Parsed, "record"))
	if q.Invalid > 0 {
		note += fmt.Sprintf(", %s not MongoDB JSON", plural(q.Invalid, "line"))
	}
	// Files whose name said log and whose contents this build cannot read. No line of theirs is
	// counted anywhere else, and a reader who is not told about them cannot know the bundle held a
	// 4.2 text log or a collector's output beside the structured ones.
	if skipped := len(q.NotRead); skipped > 0 {
		note += fmt.Sprintf(", %s named like a log and in a format this build does not read", plural(skipped, "file"))
	}
	return note
}

// categoryList is the categories a caller may name, so the flag's help is the list itself rather
// than a reference to a list somewhere else.
func categoryList() string {
	names := make([]string, 0, 12)
	for _, category := range analyze.Categories() {
		names = append(names, string(category))
	}
	return strings.Join(names, ", ")
}

// severityOf ranks the letters the server writes, worst first.
func severityOf(letter string) int {
	switch letter {
	case "F":
		return 0
	case "E":
		return 1
	case "W":
		return 2
	case "I":
		return 3
	case "D1", "D2", "D3", "D4", "D5", "D":
		return 4
	}
	return 5
}

// keepsSeverity answers --sev W with everything the server considered a warning or worse,
// because "show me the problems" is what a reader means by naming one.
func keepsSeverity(letter, wanted string) bool {
	if wanted == "" {
		return true
	}
	return severityOf(letter) <= severityOf(strings.ToUpper(wanted))
}

// matchesSource accepts a node's whole label or any part of it, so a node can be named by the
// shortest thing that tells it apart rather than in full.
func matchesSource(sources map[string]int, wanted string) bool {
	for source := range sources {
		if source == wanted || strings.Contains(source, wanted) {
			return true
		}
	}
	return false
}

// showGroup answers "what did those lines actually say", which a count of a fingerprint cannot.
func showGroup(st *store.Store, root string, c *common, group string, limit int, start, end time.Time, width int, started time.Time) error {
	sample := analyze.SampleOf(group, limit)
	if err := analyze.Run(st, windowQuery(start, end), sample); err != nil {
		return err
	}
	q, err := readQuality(st)
	if err != nil {
		return err
	}
	if len(sample.Records) == 0 {
		return emit(os.Stdout, c.json, "log_group", root,
			map[string]any{"group_id": group, "records": sample.Records, "quality": q},
			withVersion("no lines matched that group in this window"))
	}
	rendered := renderGroupLines(sample.Records, outputWidth(width))
	footer := fmt.Sprintf("%s shown of group %s; the bundle holds %s; answered in %s",
		plural(len(sample.Records), "line"), group, plural(q.Parsed, "record"), time.Since(started).Round(time.Millisecond))
	return emit(os.Stdout, c.json, "log_group", root, map[string]any{
		"group_id": group, "records": recordViews(sample.Records), "quality": q,
	}, withVersion(rendered+"\n\n"+footer))
}

// renderGroupLines prints the lines of one group the way a log is read: a header, then everything
// the line actually said, in full.
//
// This used to be a table with an Attributes column, and the column is where the answer went to
// die. The cause of a failure is often one long attribute of a fatal line — an error message that
// quotes the document it rejected — and a cell cut it short and then cut it again to fit the
// screen. Asking for a group's lines is asking to read them; nothing here is worth abbreviating.
func renderGroupLines(records []analyze.Record, width int) string {
	out := make([]string, 0, len(records)*3)
	for i, record := range records {
		if i > 0 {
			out = append(out, "")
		}
		header := fmt.Sprintf("%s  %s  %s  %s  %s",
			record.At.Format("01-02 15:04:05"), record.Sev, record.Source, record.Ctx, record.Msg)
		out = append(out, header)
		for _, field := range attributeFields(record) {
			// Wrap does not pad the first line — the caller owns the left edge — so the indent is
			// applied here, and continuation lines hang under the value rather than under the key.
			wrapped := render.Wrap(field.key+" = "+field.value, width-2, len(field.key)+3)
			for _, line := range wrapped {
				out = append(out, "  "+line)
			}
		}
	}
	return strings.Join(out, "\n")
}

// attributeField is one leaf of a log line's attribute document, whole.
type attributeField struct{ key, value string }

// attributeFields is a record's attributes flattened onto dotted keys, with nothing cut.
//
// Nested documents are flattened rather than printed whole. A log line's most useful attribute is
// usually three levels down — the server version lives in buildInfo.version — and printing the
// parent as one value put it behind a wall of siblings. Go's own %v made it worse: it renders a map
// as map[k:v k:v] in whatever order the runtime felt like, so the same line printed differently on
// each run.
//
// The cap on leaves stays — a slow query's command document runs to hundreds of fields and printing
// all of them is not reading a log line — but no value is shortened, because a value is the only
// thing on one of these lines that nothing else in the bundle says.
func attributeFields(record analyze.Record) []attributeField {
	attributes := record.Attributes()
	if len(attributes) == 0 {
		return nil
	}
	parts := make([]string, 0, 16)
	flattenWhole("", attributes, &parts)
	sort.Strings(parts)
	if len(parts) > maxAttributeLeaves {
		parts = append(parts[:maxAttributeLeaves], fmt.Sprintf("…%d more fields=", len(parts)-maxAttributeLeaves))
	}
	fields := make([]attributeField, 0, len(parts))
	for _, part := range parts {
		key, value, _ := strings.Cut(part, "=")
		fields = append(fields, attributeField{key: key, value: value})
	}
	return fields
}

// recordViews is what the JSON carries for a handful of log lines: the record, and the attributes
// it was asked for.
//
// The attributes never left the parser before. A record keeps them as raw bytes, so a caller reading
// the JSON — which is every caller driving this tool on an agent's behalf — got a timestamp, a
// severity and a message, and none of what the line actually said, which is often where the answer
// is.
func recordViews(records []analyze.Record) []map[string]any {
	out := make([]map[string]any, len(records))
	for i, record := range records {
		attributes := map[string]string{}
		for _, field := range attributeFields(record) {
			attributes[field.key] = field.value
		}
		out[i] = map[string]any{
			"at": record.At, "severity": record.Sev, "component": record.Comp,
			"id": record.LogID, "context": record.Ctx, "message": record.Msg,
			"source": record.Source, "line": record.Line, "attributes": attributes,
		}
	}
	return out
}

// maxAttributeLeaves bounds one line: a slow query's command document alone can hold hundreds of
// fields, and a screen of them is not a log line any more.
const maxAttributeLeaves = 40

// maxAttributeValue is how much of one non-string leaf is printed. Long enough for a version, a
// namespace or an error code; short enough that one field cannot take the whole line.
const maxAttributeValue = 80

// flattenWhole walks a decoded document onto "a.b.c=value" pairs, with no cut on the values: a
// caller reading a handful of lines wants the whole of each one.
func flattenWhole(prefix string, value any, into *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 0 {
			*into = append(*into, prefix+"={}")
			return
		}
		for key, nested := range typed {
			flattenWhole(join(prefix, key), nested, into)
		}
	case []any:
		if len(typed) == 0 {
			*into = append(*into, prefix+"=[]")
			return
		}
		for i, nested := range typed {
			flattenWhole(fmt.Sprintf("%s[%d]", prefix, i), nested, into)
		}
	default:
		*into = append(*into, prefix+"="+wholeLeaf(value))
	}
}

// wholeLeaf renders one scalar exactly as it was logged.
func wholeLeaf(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return leaf(value)
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// leaf prints one scalar. JSON has a single number type, so an id comes back as a float and would
// print as 2.6e+09.
func leaf(value any) string {
	text := ""
	switch typed := value.(type) {
	case float64:
		if typed == math.Trunc(typed) && math.Abs(typed) < 1e15 {
			text = strconv.FormatInt(int64(typed), 10)
		} else {
			text = strconv.FormatFloat(typed, 'g', -1, 64)
		}
	case string:
		text = typed
	case nil:
		text = "null"
	default:
		text = fmt.Sprintf("%v", typed)
	}
	if len(text) > maxAttributeValue {
		text = text[:maxAttributeValue-1] + "…"
	}
	return text
}
