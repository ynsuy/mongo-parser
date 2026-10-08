package cli

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"mongo-log-parser/internal/analyze"
	"mongo-log-parser/internal/render"
	"mongo-log-parser/internal/series"
)

func runSlow(root string, args []string) error {
	flags, c := newFlags("slow")
	w := addWindowFlags(flags)
	top := flags.Int("top", 20, "how many shapes to show")
	plan := flags.String("plan", "", "only this plan, e.g. COLLSCAN")
	namespace := flags.String("ns", "", "only this namespace")
	operation := flags.String("op", "", "only these operations: a command name, a comma-separated list, or a group ("+strings.Join(analyze.OperationGroups(), ", ")+")")
	queryHash := flags.String("query-hash", "", "only this logged queryHash or planCacheShapeHash")
	binSeconds := flags.Int64("bin", 0, "bin size in seconds for the timeline chart; chosen from the window by default")
	noChart := flags.Bool("no-chart", false, "table only, without the timeline chart above it")
	noWaits := flags.Bool("no-waits", false, "leave out the breakdown of where the time went")
	noDetail := flags.Bool("no-detail", false, "leave out the distribution and charts a filter would otherwise add")
	examples := flags.Int("examples", -1, "how many real executions of the shape to show, slowest first; 3 once a filter has narrowed the set")
	waitedOn := flags.String("waited-on", "", "only the shapes whose biggest wait is this, e.g. \"write concern\"")
	failed := flags.Bool("failed", false, "only the shapes the server rejected at least once")
	errName := flags.String("error", "", "only the shapes that failed with this error, e.g. StaleConfig")
	color := flags.String("color", "auto", "auto, always or never")
	if err := parseOnly(flags, args); err != nil {
		return err
	}
	if err := bounded(bound{"top", *top, 1}, bound{"bin", int(*binSeconds), 0}); err != nil {
		return err
	}
	// Naming a filter is what "I have found the thing, now tell me about it" looks like, so the
	// drill-down comes without having to ask for it a second time. It covers the costliest shape
	// the filter kept and no others: five shapes' worth of charts and tables is not a drill-down,
	// it is the same wall of output the filter was meant to cut through. To look at another shape,
	// narrow again with --query-hash.
	filtered := *plan != "" || *namespace != "" || *queryHash != "" || *waitedOn != "" || *operation != "" ||
		*failed || *errName != ""
	detail := filtered && !*noDetail
	if *examples < 0 {
		*examples = 0
		if detail {
			*examples = 3
		}
	}
	if err := atLeast("examples", *examples, 0); err != nil {
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

	// The examples are collected in the same pass as the shapes. A second pass to fetch three log
	// lines would cost the whole store again, and the filter that decides which lines to keep is
	// known before the pass starts.
	queries := analyze.NewSlowQueries()
	sampled := analyze.SampleShapes(analyze.ShapeFilter{
		Namespace: *namespace, Operation: *operation, Plan: *plan, Hash: *queryHash,
	}, *examples)
	sinks := []analyze.Sink{queries}
	if *examples > 0 {
		sinks = append(sinks, sampled)
	}
	if err := analyze.Run(st, windowQuery(start, end), sinks...); err != nil {
		return err
	}

	// Everything the filters kept, then the head of it for the table. The chart counts all of them:
	// "how many slow queries ran" is not a question about the twenty costliest shapes.
	matched := make([]analyze.Shape, 0, len(queries.Shapes()))
	for _, shape := range queries.Shapes() {
		if (*queryHash != "" && shape.QueryHash != *queryHash && shape.PlanCacheShapeHash != *queryHash) || (*plan != "" && shape.Plan != *plan) || (*namespace != "" && shape.Namespace != *namespace) {
			continue
		}
		if !analyze.MatchesOperation(*operation, shape.Operation) {
			continue
		}
		if *waitedOn != "" && !strings.HasPrefix(strings.ToLower(dominantWait(shape)), strings.ToLower(*waitedOn)) {
			continue
		}
		if *failed && shape.Failed == 0 {
			continue
		}
		if *errName != "" && !failedWith(shape, *errName) {
			continue
		}
		matched = append(matched, shape)
	}
	shapes := matched
	if len(shapes) > *top {
		shapes = shapes[:*top]
	}
	if len(shapes) == 0 {
		return emit(os.Stdout, c.json, "slow", root, map[string]any{
			"shapes": shapes, "total_shapes": len(queries.Shapes()), "matched_shapes": 0, "quality": q,
		}, withVersion("no slow queries matched"))
	}
	useColour := useColor(*color) && !c.json

	columns := []render.Column{
		{Title: "Namespace", Values: field(shapes, func(s analyze.Shape) string { return s.Namespace })},
		{Title: "Op", Values: field(shapes, func(s analyze.Shape) string { return s.Operation })},
		{Title: "Plan", Values: field(shapes, func(s analyze.Shape) string { return s.Plan })},
		{Title: "Count", Align: render.AlignRight, Values: field(shapes, func(s analyze.Shape) string { return strconv.Itoa(s.Count) })},
		{Title: "Total ms", Align: render.AlignRight, Values: field(shapes, func(s analyze.Shape) string { return render.Compact(s.TotalTime()) })},
		{Title: "Avg ms", Align: render.AlignRight, Values: field(shapes, func(s analyze.Shape) string { return render.Compact(s.Duration.Avg()) })},
		{Title: "P95 ms", Align: render.AlignRight, Values: field(shapes, func(s analyze.Shape) string { return render.Compact(s.Duration.Quantile(0.95)) }), Drop: 5},
		{Title: "Max ms", Align: render.AlignRight, Values: field(shapes, func(s analyze.Shape) string { return render.Compact(s.Duration.Max) })},
		{Title: "Docs", Align: render.AlignRight, Values: field(shapes, func(s analyze.Shape) string { return render.Compact(s.Docs.Avg()) }), Drop: 3},
		{Title: "Keys", Align: render.AlignRight, Values: field(shapes, func(s analyze.Shape) string { return render.Compact(s.Keys.Avg()) }), Drop: 4},
		{Title: "Ineff", Align: render.AlignRight, Values: field(shapes, func(s analyze.Shape) string {
			if !s.Reads() {
				return "-"
			}
			return render.Compact(s.Inefficiency())
		}), Drop: 2},
		{Title: "Read", Align: render.AlignRight, Values: field(shapes, func(s analyze.Shape) string { return render.Amount(s.BytesRead.Avg(), series.UnitBytes) }), Drop: 1},
		{Title: "Outcome", Values: field(shapes, func(s analyze.Shape) string { return s.Outcome() })},
		{Title: "Waited on", Values: field(shapes, dominantWait), Drop: 6},
		{Title: "Query hash", Values: field(shapes, func(s analyze.Shape) string { return s.Label() })},
	}
	options := render.TableOptions{Width: outputWidth(c.width), Color: useColour}
	rendered := render.Table(columns, options)
	// The breakdown is drawn from everything the filters kept, not from the rows above it: "where
	// did the time go" is a question about the workload, and answering it from the twenty costliest
	// shapes would leave out whatever was waiting in the tail.
	// With one shape matched and its detail below, the workload breakdown and the shape breakdown
	// are the same table, and printing both says the second one is new information.
	if !*noWaits && !(len(matched) == 1 && detail) {
		total, waits := waitTotals(matched)
		if breakdown := renderWaits(total, waits, options); breakdown != "" {
			rendered += "\n\nwhere the time went\n" + breakdown
		}
	}
	first, last := slowWindow(start, end, q, matched)
	if !*noChart && !detail {
		// With a detail view below, the timeline above the table is the same history drawn twice.
		if chart := slowTimeline(matched, first, last, *binSeconds, outputWidth(c.width), useColour); chart != "" {
			rendered = chart + "\n\n" + rendered
		}
	}
	if detail {
		lead := shapes[0]
		body := renderShapeDetail(lead, first, last, *binSeconds, outputWidth(c.width), useColour)
		if runs := sampled.Of(lead.Namespace, lead.Operation, lead.Plan, lead.ID); len(runs) > 0 {
			body += "\n\n" + plural(len(runs), "slowest logged execution") + "\n" + renderExecutions(runs, options)
		}
		rendered += "\n\n" + section(detailTitle(lead, len(matched)), body)
	}

	collscanShapes, collscanRuns := queries.Collscans()
	// Said as "of N in the bundle" when a filter is on, or the counts beside a one-row table read
	// as if they described that row.
	scope := plural(len(queries.Shapes()), "group")
	if filtered {
		scope = fmt.Sprintf("%s matched, of %s in the bundle", plural(len(matched), "group"), plural(len(queries.Shapes()), "group"))
	}
	footer := fmt.Sprintf("%s%s (%s); %d of them %s COLLSCAN (%d executions); the bundle holds %s in %s; answered in %s",
		askedWindow(start, end),
		scope, groupingNote(queries.GroupingCounts()),
		collscanShapes, are(collscanShapes), collscanRuns,
		plural(q.Parsed, "record"), plural(q.Files, "log stream"), time.Since(started).Round(time.Millisecond))
	footer += hashNote(shapes)
	if failedRuns, failedMillis, byName := queries.Failures(); failedRuns > 0 {
		share := 0.0
		if total := queries.TotalTime(); total > 0 {
			share = failedMillis / total
		}
		footer += fmt.Sprintf("\n%d of the logged operations failed and hold %.0f%% of the logged operation time (%s ms): %s. A duration says how long an operation ran, never whether it worked.",
			failedRuns, share*100, render.Compact(failedMillis), topErrors(byName))
	}
	if detail && len(matched) > 1 {
		footer += fmt.Sprintf("\nThe drill-down covers the costliest of %d matching shapes; narrow with --query-hash to look at another. --no-detail leaves it out.",
			len(matched))
	}

	waitTotal, waits := waitTotals(matched)
	// The failure totals are the bundle's, not the filtered set's: "how much of this workload
	// failed" is the question a caller asks before it reads a ranking, and answering it from the
	// rows a filter kept would answer a different one.
	failedRuns, failedMillis, failedByName := queries.Failures()
	return emit(os.Stdout, c.json, "slow", root, map[string]any{
		"shapes": shapes, "total_shapes": len(queries.Shapes()), "matched_shapes": len(matched),
		"collscan_shapes": collscanShapes, "collscan_executions": collscanRuns,
		// total_ms is the whole bundle's, matching failed_ms; wait_total_ms is the filtered set's,
		// matching waits. Reading failed_ms against the filtered total can report more than all of
		// the time as failed.
		"total_ms":          queries.TotalTime(),
		"failed_executions": failedRuns, "failed_ms": failedMillis, "failed_by_error": failedByName,
		"quality": q, "wait_total_ms": waitTotal, "waits": waits,
	}, withVersion(rendered+"\n\n"+footer))
}

// waitTotals adds up where the time of a set of shapes went. SlowQueries has the same sum over
// everything it saw; this one respects whatever the caller filtered down to.
func waitTotals(shapes []analyze.Shape) (float64, []analyze.Wait) {
	sums, seen, total := map[string]float64{}, map[string]analyze.Wait{}, 0.0
	for _, shape := range shapes {
		total += shape.Duration.Sum
		for _, wait := range shape.Waits() {
			sums[wait.Name] += wait.Millis
			seen[wait.Name] = wait
		}
	}
	if total <= 0 {
		return 0, nil
	}
	waits := make([]analyze.Wait, 0, len(sums))
	for name, millis := range sums {
		entry := seen[name]
		entry.Millis, entry.Share = millis, millis/total
		waits = append(waits, entry)
	}
	sort.Slice(waits, func(i, j int) bool {
		if waits[i].Millis != waits[j].Millis {
			return waits[i].Millis > waits[j].Millis
		}
		return waits[i].Name < waits[j].Name
	})
	return total, waits
}

// detailTitle names the section by what it is, so a reader knows what it is not: the costliest of
// several, not all of them.
func detailTitle(shape analyze.Shape, matched int) string {
	if matched == 1 {
		return "the " + shape.Operation + " in detail"
	}
	return fmt.Sprintf("the costliest %s of %s, in detail", shape.Operation, plural(matched, "matching shape"))
}

// groupingNote explains how the shapes were identified, which is what makes their number readable.
func groupingNote(counts map[string]int) string {
	parts := make([]string, 0, 3)
	for _, kind := range []string{"queryHash", "planCacheShapeHash", "namespace"} {
		if counts[kind] > 0 {
			parts = append(parts, fmt.Sprintf("%d by %s", counts[kind], kind))
		}
	}
	return strings.Join(parts, ", ")
}

func field(shapes []analyze.Shape, read func(analyze.Shape) string) []string {
	values := make([]string, len(shapes))
	for i, shape := range shapes {
		values[i] = read(shape)
	}
	return values
}

// failedWith reports whether a shape hit the named error, matched the way a reader types it: case
// insensitively, and on a prefix, so "stale" finds both StaleConfig and StaleDbVersion.
func failedWith(shape analyze.Shape, want string) bool {
	want = strings.ToLower(want)
	for name := range shape.Errors {
		if strings.HasPrefix(strings.ToLower(name), want) {
			return true
		}
	}
	return false
}

// topErrors is the three error names that failed most, with their counts.
func topErrors(byName map[string]int) string {
	type entry struct {
		name  string
		count int
	}
	entries := make([]entry, 0, len(byName))
	for name, count := range byName {
		entries = append(entries, entry{name, count})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].name < entries[j].name
	})
	parts := make([]string, 0, 3)
	for _, e := range entries[:min(3, len(entries))] {
		parts = append(parts, fmt.Sprintf("%s×%d", e.name, e.count))
	}
	return strings.Join(parts, ", ")
}
