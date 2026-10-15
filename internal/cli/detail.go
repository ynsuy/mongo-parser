package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ynsuy/mongo-parser/internal/analyze"
	"github.com/ynsuy/mongo-parser/internal/metrics"
	"github.com/ynsuy/mongo-parser/internal/render"
	"github.com/ynsuy/mongo-parser/internal/series"
)

// A table of shapes answers "which of these cost the most". Once a reader has narrowed to one, it
// answers nothing: every row is a single average, and an average is the one number that cannot say
// whether an operation is reliably slow or usually fine with a bad tail. This is the view for after
// the narrowing — the whole distribution of every measure, and what the shape did over time.

// detailChartRows is taller than the timeline above a table: this is the only chart on the page.
const detailChartRows = 5

// measure is one row of a distribution table: a name, the numbers behind it, and how to print them.
type measure struct {
	name    string
	numbers analyze.Numbers
	unit    string // series.UnitBytes, or empty for a plain number
	// core marks a measure every operation has, which is printed even when it is zero throughout.
	// A wait that never happened is worth leaving out; "this aggregate examined no documents" is
	// a fact about the operation and dropping it made the table look like it was hiding something.
	core bool
}

// renderShapeDetail is everything one shape has to say.
func renderShapeDetail(shape analyze.Shape, first, last time.Time, binSeconds int64, width int, color bool) string {
	options := render.TableOptions{Width: width, Color: color}
	parts := []string{detailHeading(shape)}
	if chart := shapeChart(shape, first, last, binSeconds, width, color); chart != "" {
		parts = append(parts, chart)
	}
	parts = append(parts, distribution(shape, options))
	if waits := shape.Waits(); len(waits) > 0 {
		if table := renderWaits(shape.Duration.Sum, waits, options); table != "" {
			parts = append(parts, "where this shape's time went\n"+table)
		}
	}
	if apps := applications(shape); apps != "" {
		parts = append(parts, apps)
	}
	return strings.Join(parts, "\n\n")
}

// detailHeading says which shape this is, in the terms a reader would use to ask for it again.
func detailHeading(shape analyze.Shape) string {
	line := fmt.Sprintf("%s · %s · plan %s · %s · %s .. %s",
		shape.Namespace, shape.Operation, shape.Plan,
		plural(shape.Count, "execution"),
		shape.First.Format("01-02 15:04"), shape.Last.Format("01-02 15:04"))
	if shape.ID == "" {
		// Saying "not logged" here matters more than in a table: a reader looking at one shape is
		// about to treat it as one query, and without a server hash it is a group of them.
		return line + "\nno query hash was logged, so this row groups every query with this namespace, operation and plan — not one query shape"
	}
	return line + "\nquery hash " + shape.Label() + " (grouped by " + shape.GroupedBy + ")"
}

// shortName drops the scaffolding a measure's row title needs and a sentence does not.
func shortName(name string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(name, "wait: "), " ms"), " µs")
}

// trivialShare is how small a wait has to be before it is named below the table rather than given a
// row in it: a thousandth of the shape's time.
const trivialShare = 0.001

// distribution is the whole spread of every measure, which is what a table of averages hides.
func distribution(shape analyze.Shape, options render.TableOptions) string {
	total := shape.Duration.Sum
	measures := []measure{
		{name: "duration ms", numbers: shape.Duration, core: true},
		{name: "docs examined", numbers: shape.Docs, core: true},
		{name: "keys examined", numbers: shape.Keys, core: true},
		{name: "docs returned", numbers: shape.Returned, core: true},
		{name: "bytes read", numbers: shape.BytesRead, unit: series.UnitBytes, core: true},
		{name: "planning µs", numbers: shape.Planning},
		{name: "wait: write concern ms", numbers: shape.WriteConcernWait},
		{name: "wait: flow control ms", numbers: shape.FlowControlWait},
		{name: "wait: lock acquire ms", numbers: shape.LockWait},
		{name: "wait: ticket queue ms", numbers: shape.QueueWait},
		{name: "wait: storage read ms", numbers: shape.StorageWait},
		{name: "wait: remote shards ms", numbers: shape.RemoteWait},
		{name: "cpu ms", numbers: shape.CPU},
	}
	kept := make([]measure, 0, len(measures))
	empty, negligible, unlogged := make([]string, 0, 4), make([]string, 0, 4), make([]string, 0, 4)
	for _, entry := range measures {
		if entry.numbers.Count == 0 {
			unlogged = append(unlogged, shortName(entry.name))
			continue
		}
		// A row of zeros is not a measurement and reading eight of them to find the two that matter
		// is why the table stopped being worth looking at. They are counted off below the table
		// instead, by name, so nothing disappears without being mentioned.
		if entry.numbers.Max == 0 {
			empty = append(empty, shortName(entry.name))
			continue
		}
		// A wait totalling under a thousandth of the time is not what made this slow. Printed at
		// full precision beside a figure in the millions it reads as a finding, which it is not.
		if !entry.core && total > 0 && entry.numbers.Sum/total < trivialShare {
			negligible = append(negligible, fmt.Sprintf("%s (%s ms in total)",
				shortName(entry.name), render.Compact(entry.numbers.Sum)))
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) == 0 {
		return "the log reported no measurements for this shape"
	}
	amount := func(value float64, unit string) string {
		if unit != "" {
			return render.Amount(value, unit)
		}
		return render.Compact(value)
	}
	column := func(read func(measure) string) []string {
		values := make([]string, len(kept))
		for i, entry := range kept {
			values[i] = read(entry)
		}
		return values
	}
	table := render.Table([]render.Column{
		{Title: "Measure", Values: column(func(m measure) string { return m.name })},
		{Title: "N", Align: render.AlignRight, Values: column(func(m measure) string { return strconv.Itoa(m.numbers.Count) })},
		{Title: "Min", Align: render.AlignRight, Values: column(func(m measure) string { return amount(m.numbers.Min, m.unit) })},
		{Title: "Avg", Align: render.AlignRight, Values: column(func(m measure) string { return amount(m.numbers.Avg(), m.unit) })},
		{Title: "P50", Align: render.AlignRight, Values: column(func(m measure) string { return amount(m.numbers.Quantile(0.50), m.unit) })},
		{Title: "P95", Align: render.AlignRight, Values: column(func(m measure) string { return amount(m.numbers.Quantile(0.95), m.unit) })},
		{Title: "P99", Align: render.AlignRight, Values: column(func(m measure) string { return amount(m.numbers.Quantile(0.99), m.unit) })},
		{Title: "Max", Align: render.AlignRight, Values: column(func(m measure) string { return amount(m.numbers.Max, m.unit) })},
		{Title: "Total", Align: render.AlignRight, Values: column(func(m measure) string { return amount(m.numbers.Sum, m.unit) })},
	}, options)
	notes := []string{table, "Min, max, count and total are exact; the percentiles are approximate."}
	if len(empty) > 0 {
		notes = append(notes, "zero throughout, so not shown: "+strings.Join(empty, ", ")+
			" (measured zero, or not a field this server writes here)")
	}
	if len(negligible) > 0 {
		notes = append(notes, "under a thousandth of the time, so not shown: "+strings.Join(negligible, ", "))
	}
	if len(unlogged) > 0 {
		notes = append(notes, "in none of its lines, so unknown rather than zero: "+strings.Join(unlogged, ", "))
	}
	return strings.Join(notes, "\n")
}

// applications names the clients that ran this, which is how a shape becomes a team to talk to.
func applications(shape analyze.Shape) string {
	if len(shape.Apps) == 0 {
		return ""
	}
	parts := make([]string, 0, len(shape.Apps))
	for app, count := range shape.Apps {
		parts = append(parts, fmt.Sprintf("%s×%d", app, count))
	}
	sort.Strings(parts)
	return "applications: " + strings.Join(parts, " ")
}

// chartableWait is how much of a shape's time has to be waiting before the wait earns a line of its
// own on the chart. Below it the line sits on the floor and adds nothing but a legend entry.
const chartableWait = 0.02

// waitTotalOf is every wait this shape reported, added up. cpu is work and not counted.
func waitTotalOf(shape analyze.Shape) float64 {
	return shape.FlowControlWait.Sum + shape.LockWait.Sum + shape.WriteConcernWait.Sum +
		shape.RemoteWait.Sum + shape.StorageWait.Sum + shape.QueueWait.Sum
}

// shapeChart draws what this shape did over time: how often it ran, and what it cost when it did.
//
// Executions and duration are the two questions and they are not the same one. A shape whose count
// rose is a workload change; a shape whose count held steady while its duration climbed is the
// server getting slower underneath it, and the table above cannot separate them because it has
// added the whole window into one row.
func shapeChart(shape analyze.Shape, first, last time.Time, binSeconds int64, width int, color bool) string {
	if len(shape.Timeline) == 0 || !last.After(first) {
		return ""
	}
	if binSeconds <= 0 {
		binSeconds = metrics.AutoBucketSecondsFor(last.Sub(first), analyze.TimelineStep, render.BarColumns(width))
	}
	folded := analyze.Timeline{}
	shape.Timeline.Fold(binSeconds, folded)

	from, to := float64(alignDown(first, binSeconds).UnixMilli()), float64(alignDown(last, binSeconds).UnixMilli())

	// A panel each, rather than lines sharing one. Milliseconds and a count share no axis, and the
	// wait shares an axis with the duration but not a question: on the duration panel it was a
	// fourth line in a legend that had already run out of room, and its own shape — steady drip or
	// one bad hour — was the thing it was drawn to show.
	cost := barPanel([]series.Series{
		binSeries("max ms", folded, func(bin analyze.Bin) float64 { return bin.Max }, first, last, binSeconds),
		binSeries("avg ms", folded, analyze.Bin.Avg, first, last, binSeconds),
		binSeries("min ms", folded, func(bin analyze.Bin) float64 { return bin.Min }, first, last, binSeconds),
	}, render.BarOptions{
		Width: width, Height: detailChartRows, First: from, Last: to,
		Title: fmt.Sprintf("Duration per %s  ·  avg %s ms over the window, worst %s ms",
			render.BinLabel(binSeconds), render.Compact(shape.Duration.Avg()), render.Compact(shape.Duration.Max)),
	})
	out := []string{cost}

	if waited := waitTotalOf(shape); shape.Duration.Sum > 0 && waited/shape.Duration.Sum >= chartableWait {
		out = append(out, barPanel([]series.Series{
			binSeries("avg wait ms", folded, analyze.Bin.AvgWait, first, last, binSeconds),
			binSeries("avg ms", folded, analyze.Bin.Avg, first, last, binSeconds),
		}, render.BarOptions{
			Width: width, Height: detailChartRows, First: from, Last: to,
			// The duration is kept on this panel as the second line, because a wait means nothing
			// on its own: 1.5 seconds of waiting is everything on a 1.6-second operation and
			// nothing on a minute-long one. The gap between the two lines is the work.
			Title: fmt.Sprintf("Waiting per %s  ·  %.0f%% of this shape's time, %s ms of %s",
				render.BinLabel(binSeconds), 100*waited/shape.Duration.Sum,
				render.Compact(waited), render.Compact(shape.Duration.Sum)),
		}))
	}

	runs := barPanel([]series.Series{
		binSeries("executions", folded, func(bin analyze.Bin) float64 { return float64(bin.Count) }, first, last, binSeconds),
	}, render.BarOptions{
		Width: width, Height: detailChartRows - 1, First: from, Last: to,
		Title: fmt.Sprintf("Executions per %s  ·  %s in the window", render.BinLabel(binSeconds), render.Compact(float64(shape.Count))),
	})
	out = append(out, runs)
	return strings.Join(out, "\n")
}
