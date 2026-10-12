package cli

import (
	"fmt"
	"math"
	"sort"
	"time"

	"mongo-log-parser/internal/analyze"
	"mongo-log-parser/internal/metrics"
	"mongo-log-parser/internal/render"
	"mongo-log-parser/internal/series"
)

// slowChartRows is the plot height the timeline gets. It is one chart on a page of table, so it can
// afford more rows than a panel on a board of them.
const slowChartRows = 7

// slowTimeline draws when the slow queries happened, above the table that says what they were.
//
// The table ranks shapes by what they cost over the whole window, which is the right first
// question and cannot answer the second one: four minutes of server time spread evenly over two
// days is a query that wants an index, and the same four minutes inside one ten-minute window is an
// incident, and the table prints them identically. The chart is that difference.
//
// The lines are the plan summaries, because that is the split that changes what you do about it: a
// wall of COLLSCAN at 03:00 is a nightly job reading a collection end to end, and the same count of
// IXSCAN is load.
func slowTimeline(shapes []analyze.Shape, first, last time.Time, binSeconds int64, width int, color bool) string {
	if len(shapes) == 0 || !last.After(first) {
		return ""
	}
	slots := render.PanelBins(width)
	if binSeconds <= 0 {
		binSeconds = metrics.AutoBucketSecondsFor(last.Sub(first), analyze.TimelineStep, slots)
	}

	byPlan := map[string]analyze.Timeline{}
	for _, shape := range shapes {
		if byPlan[shape.Plan] == nil {
			byPlan[shape.Plan] = analyze.Timeline{}
		}
		shape.Timeline.Fold(binSeconds, byPlan[shape.Plan])
	}
	// Counted off the folded histories rather than off the shapes, so the figure in the title is
	// the figure the lines add up to. A window that clipped part of a shape would otherwise be
	// titled with executions the chart does not show.
	total := 0
	for _, timeline := range byPlan {
		total += countOf(timeline)
	}

	plans := make([]string, 0, len(byPlan))
	for plan := range byPlan {
		plans = append(plans, plan)
	}
	// Busiest plan first, so the legend order is the order they matter in and the ties break the
	// same way every run.
	sort.Slice(plans, func(i, j int) bool {
		left, right := countOf(byPlan[plans[i]]), countOf(byPlan[plans[j]])
		if left != right {
			return left > right
		}
		return plans[i] < plans[j]
	})

	rows := make([]series.Series, 0, len(plans))
	for _, plan := range plans {
		rows = append(rows, countSeries(planLabel(plan), byPlan[plan], first, last, binSeconds))
	}

	// The total is a figure in the title rather than a line on the chart. Drawn, it spends most of
	// its length in the same cells as whichever plan dominates — a chart of overlap marks — and it
	// says nothing the top of the lines below it does not already say.
	from, to := float64(alignDown(first, binSeconds).UnixMilli()), float64(alignDown(last, binSeconds).UnixMilli())
	chart := gnuplotPanel(rows, render.PanelOptions{
		Width: width, Height: slowChartRows, Color: color, First: from, Last: to,
		Title: fmt.Sprintf("Slow queries per %s  ·  %s in the window",
			render.BinLabel(binSeconds), render.Compact(float64(total))),
	})
	return chart
}

// planLabel is what the legend calls a plan. A shape whose log line carried no plan summary is
// filed under "-", which is a hyphen in a legend and a puzzle to the reader.
func planLabel(plan string) string {
	if plan == "-" || plan == "" {
		return "no plan"
	}
	return plan
}

func countOf(timeline analyze.Timeline) int {
	total := 0
	for _, bin := range timeline {
		total += bin.Count
	}
	return total
}

// countSeries turns a folded history into a series of execution counts.
func countSeries(label string, timeline analyze.Timeline, first, last time.Time, binSeconds int64) series.Series {
	return binSeries(label, timeline, func(bin analyze.Bin) float64 { return float64(bin.Count) }, first, last, binSeconds)
}

// binSeries turns a folded history into the series a chart draws, reading one number out of each
// bin: how many ran, what the average one cost, what the worst one cost.
//
// Every bin in the window is emitted, including the empty ones. A minute in which no slow query was
// logged is a zero and not a gap: the server was running and nothing was slow, which is the whole
// point of looking. Left absent they would be drawn as missing data, which claims the opposite.
func binSeries(label string, timeline analyze.Timeline, read func(analyze.Bin) float64, first, last time.Time, binSeconds int64) series.Series {
	start, end := alignDown(first, binSeconds).Unix(), alignDown(last, binSeconds).Unix()
	buckets := make([]series.Bucket, 0, (end-start)/binSeconds+1)
	values := make([]float64, 0, cap(buckets))
	peak, peakAt := math.Inf(-1), start
	var sum float64
	for at := start; at <= end; at += binSeconds {
		count := read(timeline[at])
		buckets = append(buckets, series.Bucket{
			Start:   time.Unix(at, 0).UTC().Format(time.RFC3339),
			Samples: 1, Min: count, Avg: count, Max: count,
		})
		values = append(values, count)
		sum += count
		if count > peak {
			peak, peakAt = count, at
		}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return series.Series{
		Metric: label, Label: label, Mode: "count", BucketSeconds: binSeconds,
		SampleCount: len(values), Buckets: buckets,
		Stats: &series.Stats{
			Min: sorted[0], Avg: sum / float64(len(sorted)), Median: sorted[len(sorted)/2],
			P95: sorted[int(math.Ceil(float64(len(sorted))*0.95))-1], Max: sorted[len(sorted)-1],
			Peak: &series.Peak{Timestamp: time.Unix(peakAt, 0).UTC().Format(time.RFC3339), Value: peak},
		},
	}
}

// alignDown puts a moment on the bin grid, so every line starts in the same column as the ruler.
func alignDown(at time.Time, seconds int64) time.Time {
	if seconds <= 0 {
		seconds = analyze.TimelineStep
	}
	return time.Unix(at.Unix()/seconds*seconds, 0).UTC()
}

// slowWindow is the span the chart covers: what the caller asked for, or what the logs turned out
// to hold.
func slowWindow(start, end time.Time, q quality, shapes []analyze.Shape) (first, last time.Time) {
	first, last = start, end
	if first.IsZero() {
		first = q.FirstAt
	}
	if last.IsZero() {
		last = q.LastAt
	}
	// A store that read no timestamps at all still has the shapes' own extent to fall back on.
	if first.IsZero() || last.IsZero() {
		for _, shape := range shapes {
			if first.IsZero() || (!shape.First.IsZero() && shape.First.Before(first)) {
				first = shape.First
			}
			if shape.Last.After(last) {
				last = shape.Last
			}
		}
	}
	return first, last
}
