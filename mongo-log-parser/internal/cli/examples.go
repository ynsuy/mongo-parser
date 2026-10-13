package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"mongo-log-parser/internal/analyze"
	"mongo-log-parser/internal/render"
)

// The distribution says a shape's slowest execution took five seconds. The next question is always
// the same one — show me that execution — and a count of a fingerprint cannot answer it. These are
// the real lines, slowest first.

// renderExecutions shows sampled executions of one shape.
func renderExecutions(executions []analyze.Execution, options render.TableOptions) string {
	if len(executions) == 0 {
		return ""
	}
	text := func(read func(analyze.Execution) string) []string {
		values := make([]string, len(executions))
		for i, entry := range executions {
			values[i] = read(entry)
		}
		return values
	}
	table := render.Table([]render.Column{
		{Title: "When", Values: text(func(e analyze.Execution) string { return e.At.Format("01-02 15:04:05") })},
		{Title: "Node", Align: render.AlignTail, Values: text(func(e analyze.Execution) string { return e.Source })},
		{Title: "ms", Align: render.AlignRight, Values: text(func(e analyze.Execution) string { return render.Compact(e.Millis) })},
		{Title: "Spent on", Values: text(waitSummary)},
		{Title: "Docs", Align: render.AlignRight, Values: text(func(e analyze.Execution) string { return loggedValue(e.Docs) })},
		{Title: "Keys", Align: render.AlignRight, Values: text(func(e analyze.Execution) string { return loggedValue(e.Keys) })},
		{Title: "Ret", Align: render.AlignRight, Values: text(func(e analyze.Execution) string { return loggedValue(e.Returned) })},
		{Title: "App", Values: text(func(e analyze.Execution) string { return orDash(e.AppName) })},
	}, options)

	// The command goes under the table rather than in it: it is the longest thing on the line and
	// the most useful, and squeezed into a column it would be cut to nothing.
	lines := []string{table}
	for _, entry := range executions {
		rendered := command(entry.Command, entry.Operation, options.Width-6)
		if rendered == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s  %s ms\n    %s",
			entry.At.Format("01-02 15:04:05"), render.Compact(entry.Millis), rendered))
	}
	return strings.Join(lines, "\n")
}

// notableWait is the smallest share of an execution worth naming as what it waited for. A
// microsecond of flow control on a five-second operation printed as "flow control 0%", which reads
// as a finding and is the opposite of one.
const notableWait = 0.01

// waitSummary is the two largest things one execution spent its time on, for a column.
func waitSummary(execution analyze.Execution) string {
	names := make([]string, 0, len(execution.Waits))
	for name := range execution.Waits {
		if name == "cpu" || execution.Millis <= 0 || execution.Waits[name]/execution.Millis < notableWait {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "-"
	}
	sort.Slice(names, func(i, j int) bool { return execution.Waits[names[i]] > execution.Waits[names[j]] })
	parts := make([]string, 0, 2)
	for _, name := range names[:min(2, len(names))] {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", name, 100*execution.Waits[name]/execution.Millis))
	}
	return strings.Join(parts, ", ")
}

// loggedValue is a value the line may not have carried: "-" when it did not.
func loggedValue(value *float64) string {
	if value == nil {
		return "-"
	}
	return render.Compact(*value)
}

// commandEnvelope are the fields every command carries and nobody reads: session ids, cluster
// times, the routing metadata a mongos attaches, the audit block an enterprise build prepends.
//
// They come first in the document as logged, so a command printed as-is and cut to a terminal width
// showed the envelope and none of the query. The query predicate is the entire reason to print the
// command at all.
var commandEnvelope = map[string]bool{
	"lsid": true, "txnNumber": true, "autocommit": true, "startTransaction": true,
	"shardVersion": true, "databaseVersion": true, "clientOperationKey": true,
	"mayBypassWriteBlocking": true, "apiVersion": true, "apiStrict": true,
	"apiDeprecationErrors": true, "needsMerge": true, "fromMongos": true, "let": true,
}

// operative keeps the part of a command document that says what it did: the command name first,
// then its arguments, with the envelope dropped. Fields beginning with $ are all metadata.
func operative(document map[string]any, operation string) []string {
	keys := make([]string, 0, len(document))
	for key := range document {
		if strings.HasPrefix(key, "$") || commandEnvelope[key] {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// The command names itself with its first field, and a reader looks for that first. Sorting
	// alphabetically buried "delete" under "deletes" and "comment".
	sort.SliceStable(keys, func(i, j int) bool { return keys[i] == operation && keys[j] != operation })
	return keys
}

// command renders the command document as the server logged it, with the envelope dropped and the
// command's own name first.
func command(raw json.RawMessage, operation string, width int) string {
	if len(raw) == 0 {
		return ""
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil {
		return ""
	}
	keys := operative(document, operation)
	if len(keys) == 0 {
		return ""
	}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		compact, err := json.Marshal(document[key])
		if err != nil {
			parts = append(parts, key+": <value>")
			continue
		}
		parts = append(parts, key+": "+string(compact))
	}
	text := "{" + strings.Join(parts, ", ") + "}"
	if width > 20 && len(text) > width {
		text = text[:width-1] + "…"
	}
	return text
}
