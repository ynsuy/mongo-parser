package render

import (
	"math"
	"strconv"
	"strings"
	"time"

	"mongo-log-parser/internal/series"
)

// ReportOptions controls how much room a set of metrics gets, and whether it is coloured.
type ReportOptions struct {
	Width int
	// ChartHeight is the number of plot rows drawn under each metric's row.
	ChartHeight int
	Color       bool
}

// MetricsReport is one table: a row of numbers per metric with that metric's chart directly
// underneath it, so the values line up down the columns and each chart stays with its own row.
func MetricsReport(rows []series.Series, options ReportOptions) string {
	if len(rows) == 0 {
		return "No metrics to show."
	}
	width := max(48, options.Width)
	height := max(2, options.ChartHeight)
	paint := painter(options.Color)

	colors := sourceColors(rows)
	scales := sharedScales(rows)
	// One clock for the whole table: the rows under it are one period, whatever each stream
	// happened to cover of it.
	first, last := overlaySpan(rows)
	columns := statColumns(rows)
	// A narrow terminal loses the least diagnostic columns before the table is allowed to overflow.
	for requiredTableWidth(columns) > width {
		droppable := -1
		for i, column := range columns {
			if column.drop > 0 && (droppable < 0 || column.drop < columns[droppable].drop) {
				droppable = i
			}
		}
		if droppable < 0 {
			break
		}
		columns = append(columns[:droppable:droppable], columns[droppable+1:]...)
	}
	widths := summaryWidths(columns)
	// The metric column is as wide as the longest name on the board: every path is printed whole,
	// and only a terminal too narrow to hold the numbers shortens it.
	fixed := tableWidth(widths) - widths[0]
	widths[0] = max(nameSqueeze, min(widths[0], width-fixed))
	inner := tableWidth(widths) - 4 // a chart spans the row, minus the frame and its padding

	border := paint(tableBorder(widths), dim)
	pipe := paint("|", dim)
	row := func(values []string, style ansi, source ansi) string {
		cells := make([]string, len(values))
		for i, value := range values {
			cell := Fit(value, widths[i], columns[i].align)
			if i == 0 && columns[i].title == "Source" && source != plain {
				cells[i] = paint(cell, source)
				continue
			}
			cells[i] = paint(cell, style)
		}
		return pipe + " " + strings.Join(cells, " "+pipe+" ") + " " + pipe
	}
	titles := make([]string, len(columns))
	for i, column := range columns {
		titles[i] = column.title
	}

	out := make([]string, 0, len(rows)*(height+4)+6)
	out = append(out, paint(strings.Join(pack(reportHeader(rows, first, last), tableWidth(widths)), "\n"), dim))
	// Which colour is which node, said once, above the rows that use it.
	if line := Sources(rows, options.Color); line != "" {
		out = append(out, line)
	}
	out = append(out, border, row(titles, bold, plain), border)
	for i, entry := range rows {
		values := make([]string, len(columns))
		for column := range columns {
			values[column] = columns[column].values[i]
		}
		out = append(out, row(values, plain, colors[entry.Source]))
		lo, hi := scales[entry.Metric][0], scales[entry.Metric][1]
		for _, line := range chartLines(entry, inner, height, lo, hi, first, last) {
			out = append(out, pipe+" "+paint(Fit(line, inner, AlignLeft), colors[entry.Source])+" "+pipe)
		}
		out = append(out, border)
	}
	return strings.Join(out, "\n")
}

func tableBorder(widths []int) string {
	border := "+"
	for _, w := range widths {
		border += strings.Repeat("-", w+2) + "+"
	}
	return border
}

// chartLines is the picture under a metric's numbers, drawn against the value range it shares with
// the other sources of the same metric.
//
// The value axis takes the same room under every row. Sizing it per chart let a metric that reaches
// 10 start its plot four columns left of one that reaches 0.750, and a plot that moves sideways
// between rows reads as data that moved.
func chartLines(row series.Series, width, height int, lo, hi, first, last float64) []string {
	if len(row.Buckets) == 0 {
		return []string{"no samples in this window"}
	}
	return axisChart(row, width, height, lo, hi, MaxAxisGutter, first, last)
}

// sharedScales gives every metric one value range across the sources that reported it. Two nodes
// drawn to their own scales look alike however far apart their numbers are, which is the opposite
// of what a reader comparing them needs; a metric reported by one source keeps its own range.
func sharedScales(rows []series.Series) map[string][2]float64 {
	scales := make(map[string][2]float64, len(rows))
	for i := range rows {
		lo, hi := rows[i].Range()
		if seen, ok := scales[rows[i].Metric]; ok {
			lo, hi = math.Min(lo, seen[0]), math.Max(hi, seen[1])
		}
		scales[rows[i].Metric] = [2]float64{lo, hi}
	}
	return scales
}

func reportHeader(rows []series.Series, first, last float64) []string {
	meta := make([]string, 0, 4)
	// The window the charts below were drawn against, which is every stream's window and not the
	// first one's: a header naming one node's coverage described a table that showed all of them.
	if last > first {
		meta = append(meta, stamp(first)+" -> "+stamp(last))
	} else {
		for _, row := range rows {
			if len(row.Buckets) > 0 {
				meta = append(meta, row.Buckets[0].Start+" -> "+row.Buckets[len(row.Buckets)-1].Start)
				break
			}
		}
	}
	for _, row := range rows {
		if row.BucketSeconds != 0 {
			meta = append(meta, BinLabel(row.BucketSeconds)+"/bin")
			break
		}
	}
	return append(meta, binNote(rows), "· = gap")
}

// binNote says which end of a bin the line was drawn from. A pool is drawn from its trough, and a
// board that mixes the two has to say so: a reader comparing a flat line against a dipping one is
// owed the fact that they are not the same reduction.
func binNote(rows []series.Series) string {
	pools, peaks := false, false
	for _, row := range rows {
		if row.Low {
			pools = true
		} else {
			peaks = true
		}
	}
	switch {
	case pools && peaks:
		return "peak per bin, trough per bin for what runs out (available)"
	case pools:
		return "trough per bin"
	default:
		return "peak per bin"
	}
}

// statColumns builds the row of numbers each metric is summarized by. When the metrics come from
// more than one stream the source becomes a column of its own: a shared header would claim they
// all came from the first one.
func statColumns(rows []series.Series) []summaryColumn {
	sources := make([]string, len(rows))
	mixed := false
	for i, entry := range rows {
		sources[i] = entry.Source
		mixed = mixed || entry.Source != rows[0].Source
	}
	names := make([]string, len(rows))
	modes := make([]string, len(rows))
	stats := make([][]string, 5)
	for i := range stats {
		stats[i] = make([]string, len(rows))
	}
	samples := make([]string, len(rows))
	peaks := make([]string, len(rows))
	for i, entry := range rows {
		names[i] = nameOf(entry)
		modes[i] = entry.Kind()
		for column, value := range statValues(entry.Stats, entry.Unit) {
			stats[column][i] = value
		}
		samples[i] = "-"
		if entry.SampleCount > 0 {
			samples[i] = strconv.Itoa(entry.SampleCount)
		}
		peaks[i] = peakTime(entry.Stats)
	}
	columns := []summaryColumn{
		{title: "Metric", align: AlignTail, values: names},
		{title: "Mode", align: AlignLeft, values: modes, drop: 5},
		{title: "Min", align: AlignRight, values: stats[0], drop: 4},
		{title: "Avg", align: AlignRight, values: stats[1]},
		{title: "Median", align: AlignRight, values: stats[2], drop: 1},
		{title: "P95", align: AlignRight, values: stats[3], drop: 6},
		{title: "Max", align: AlignRight, values: stats[4]},
		{title: "N", align: AlignRight, values: samples, drop: 2},
		{title: "Peak", align: AlignRight, values: peaks, drop: 3},
	}
	if mixed {
		columns = append([]summaryColumn{{title: "Source", align: AlignTail, values: sources}}, columns...)
	}
	return columns
}

// stamp is a moment in epoch milliseconds, in the form the buckets are labelled with.
func stamp(millis float64) string {
	return time.UnixMilli(int64(millis)).UTC().Format(time.RFC3339)
}
