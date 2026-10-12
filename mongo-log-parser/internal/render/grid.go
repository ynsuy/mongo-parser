package render

import (
	"strings"

	"mongo-log-parser/internal/series"
)

// GridOptions is how a board of small charts is laid out: one row per metric, one column per
// stream, and every panel the same size on every screen.
type GridOptions struct {
	Width       int
	ChartHeight int // plot rows inside every panel
	Color       bool
	Sources     []string // the streams to give a column, in the order they get one
	// Selected is the metric row under the cursor, or -1 when nothing is selected. A board that is
	// scrolled through needs to say where the cursor is, or the keys that move it do nothing
	// visible until the screen changes underneath.
	Selected int
}

const (
	// gridNameColumn is the room the metric column takes. It is fixed rather than sized to the
	// longest name so that stepping from one group to the next does not move every panel sideways.
	gridNameColumn = 22

	// minPanelWidth is the narrowest a panel may be and still say anything: the fixed value axis,
	// its bracket, and enough columns to show a shape rather than a smudge.
	minPanelWidth = MaxAxisGutter + 2 + 16
)

// GridPanels is how many stream columns a grid of this width can hold at a readable size. A caller
// with more streams than that pages through them: shrinking every panel to fit nine nodes gives
// nine charts that none of them can be read from.
func GridPanels(width int) int {
	usable := max(0, width-1-(gridNameColumn+3))
	return max(1, usable/(minPanelWidth+3))
}

// gridWidths splits a terminal into the metric column and the equal panels beside it.
func gridWidths(width, panels int) (name, panel int) {
	panels = max(1, panels)
	name = gridNameColumn
	panel = (width - 1 - (name + 3) - 3*panels) / panels
	if panel < minPanelWidth {
		// Too narrow to give every panel its floor and the metric column its usual room. The
		// panels keep the floor and the column gives way: a name can be read cut short, a chart
		// squeezed to nine columns cannot be read at all.
		panel = minPanelWidth
		name = max(12, width-1-3-panels*(panel+3))
	}
	// Whatever the division left over widens the metric column rather than sitting as a ragged
	// edge past the last panel.
	if spare := width - 1 - (name + 3) - panels*(panel+3); spare > 0 {
		name += spare
	}
	return name, panel
}

// GridSlots is how many plot columns each panel of such a grid gets, so a caller can bin its
// series to the width they will actually be drawn at rather than to the whole terminal.
func GridSlots(width, panels int) int {
	_, panel := gridWidths(max(48, width), panels)
	return max(8, panel-MaxAxisGutter-2)
}

// MetricGrid draws a metric per row and a stream per column: the small-multiples answer to several
// nodes on one screen.
//
// Two nodes never share a cell, so no line is hidden behind another one — which is what an overlay
// of three nodes always ends up doing, since only one of them can own a column. The value axis is
// shared down a row, so panels stay comparable by height alone, and the time axis is shared by the
// whole board, so the same column is the same moment in every panel on the screen.
func MetricGrid(rows []series.Series, options GridOptions) string {
	if len(rows) == 0 {
		return "No metrics to show."
	}
	paint := painter(options.Color)
	colors, pens := sourceColors(rows), sourceStrokes(rows)

	sources := options.Sources
	if len(sources) == 0 {
		sources = sourcesOf(rows)
	}
	if len(sources) == 0 {
		sources = []string{""}
	}
	order, byMetric := byMetricSource(rows)

	name, panel := gridWidths(max(48, options.Width), len(sources))
	height := max(2, options.ChartHeight)
	slots := max(8, panel-MaxAxisGutter-2)
	first, last := overlaySpan(rows)

	widths := make([]int, 0, len(sources)+1)
	widths = append(widths, name)
	for range sources {
		widths = append(widths, panel)
	}
	border := paint(tableBorder(widths), dim)
	pipe := paint("|", dim)

	out := make([]string, 0, len(order)*(height+4)+4)
	head := make([]string, 0, len(sources)+1)
	head = append(head, paint(Fit("Metric", name, AlignLeft), bold))
	for _, source := range sources {
		head = append(head, paint(Fit(source, panel, AlignLeft), colors[source]))
	}
	out = append(out, border, pipe+" "+strings.Join(head, " "+pipe+" ")+" "+pipe, border)

	for index, metric := range order {
		cells := make([][]string, 0, len(sources)+1)
		lo, hi := metricScale(byMetric[metric])
		named, styles := gridNameCell(byMetric[metric], sources, name, lo, hi, index == options.Selected)
		cells = append(cells, named)
		for _, source := range sources {
			cells = append(cells, gridPanel(byMetric[metric][source], panel, height, slots, lo, hi, first, last, pens[source]))
		}
		for line := range height + 2 {
			row := make([]string, len(cells))
			for column, cell := range cells {
				text := ""
				if line < len(cell) {
					text = cell[line]
				}
				// Colour goes on after the padding, or the escape codes take part in the width
				// arithmetic and every column below drifts.
				text = Fit(text, widths[column], AlignLeft)
				style := plain
				if column > 0 {
					style = colors[sources[column-1]]
				} else if line < len(styles) {
					style = styles[line]
				}
				row[column] = paint(text, style)
			}
			out = append(out, pipe+" "+strings.Join(row, " "+pipe+" ")+" "+pipe)
		}
		out = append(out, border)
	}
	// One time axis under the whole board: every panel above it covers the same window.
	ticks := make([]string, 0, len(sources)+1)
	ticks = append(ticks, strings.Repeat(" ", name))
	for range sources {
		ticks = append(ticks, Fit(strings.Repeat(" ", MaxAxisGutter+2)+TimeAxis(rows, slots), panel, AlignLeft))
	}
	return strings.Join(append(out, paint("  "+strings.Join(ticks, "   "), dim)), "\n")
}

// byMetricSource indexes a board by metric and then by stream, keeping the order the metrics were
// handed over in: that order is the ranking, and sorting it away would throw the ranking away.
func byMetricSource(rows []series.Series) ([]string, map[string]map[string]series.Series) {
	order := make([]string, 0, len(rows))
	byMetric := make(map[string]map[string]series.Series, len(rows))
	for _, row := range rows {
		if _, seen := byMetric[row.Metric]; !seen {
			byMetric[row.Metric] = map[string]series.Series{}
			order = append(order, row.Metric)
		}
		byMetric[row.Metric][row.Source] = row
	}
	return order, byMetric
}

// metricScale is the value range every panel in one row is drawn against.
func metricScale(bySource map[string]series.Series) (lo, hi float64) {
	rows := make([]series.Series, 0, len(bySource))
	for _, row := range bySource {
		rows = append(rows, row)
	}
	return overlayScale(rows)
}

// gridNameCell is the left-hand column of one row: what the metric is called, how it should be
// read, and the range the panels beside it are drawn against. It hands back the styles beside the
// text rather than painted text, so the caller can pad first and colour after.
func gridNameCell(bySource map[string]series.Series, sources []string, width int, lo, hi float64, selected bool) (lines []string, styles []ansi) {
	var any series.Series
	for _, source := range sources {
		if row, ok := bySource[source]; ok {
			any = row
			break
		}
	}
	if any.Metric == "" {
		for _, row := range bySource {
			any = row
			break
		}
	}
	marker, style := "  ", plain
	if selected {
		marker, style = "> ", bold
	}
	return []string{
		marker + Fit(nameOf(any), max(1, width-2), AlignTail),
		"  " + any.Kind(),
		"  " + Amount(lo, any.Unit) + " - " + Amount(hi, any.Unit),
	}, []ansi{style, dim, dim}
}

// gridPanel is one stream's chart for one metric: the fixed value axis, the plot, its baseline and
// that stream's own numbers under it.
//
// Each stream keeps the glyph set the legend named it with, so a board printed without colour still
// says which column belongs to which node.
func gridPanel(row series.Series, width, height, slots int, lo, hi, first, last float64, pen strokes) []string {
	gutter := MaxAxisGutter
	if len(row.Buckets) == 0 {
		lines := make([]string, height+2)
		lines[height/2] = strings.Repeat(" ", gutter+2) + "no samples"
		return lines
	}
	labels, _ := axisLabels(lo, hi, height, row.Unit)
	plot := plotStrokes(row, slots, height, lo, hi, first, last, pen)
	lines := make([]string, 0, height+2)
	for line, label := range labels {
		lines = append(lines, Fit(label, gutter, AlignRight)+" |"+plot[line])
	}
	drawn := len([]rune(plot[0]))
	lines = append(lines, strings.Repeat(" ", gutter)+" +"+strings.Repeat("-", drawn))
	return append(lines, Fit(panelStats(row, width), width, AlignLeft))
}

// panelStats is the shortest honest summary of one panel: what it reached, and what it sat at.
//
// A narrow panel drops the least diagnostic number rather than printing an ellipsis: "max 120
// p95 120 av…" spends a whole line saying nothing about the value it cut in half.
func panelStats(row series.Series, width int) string {
	if row.Stats == nil {
		return ""
	}
	max, p95, avg := Amount(row.Stats.Max, row.Unit), Amount(row.Stats.P95, row.Unit), Amount(row.Stats.Avg, row.Unit)
	for _, form := range []string{
		"  max " + max + "   p95 " + p95 + "   avg " + avg,
		"  max " + max + "  p95 " + p95 + "  avg " + avg,
		"  max " + max + "  p95 " + p95,
		"  max " + max,
	} {
		if len([]rune(form)) <= width {
			return form
		}
	}
	return "  " + max
}
