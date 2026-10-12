package render

import (
	"math"
	"strings"
	"unicode/utf8"

	"mongo-log-parser/internal/series"
)

// PanelOptions is how much room a panel gets and what it is called.
type PanelOptions struct {
	Width int
	// Height is the plot rows, and it is the whole panel's plot rather than a share of it per
	// series: every metric on the panel is drawn on one chart, against one value axis. A panel of
	// six metrics is therefore exactly as tall as a panel of two, which is the guarantee that
	// matters when a board is scrolled — a shape means the same thing wherever it is drawn, and
	// the layout can be worked out before anything is rendered.
	Height int
	Color  bool
	Title  string
	// Selected draws the cursor marker. A board scrolls under the cursor, and a cursor nothing
	// shows is a key that appears to do nothing.
	Selected bool
	// First and Last are the window every panel on the board covers, in epoch milliseconds. Panels
	// share it so the same column is the same moment down the whole screen.
	First, Last float64
	//
	// A terminal column stands for half an hour of a three-day bundle, and the two readings of that
	// half hour are different pictures. The peak is the default because this tool is for finding
	// problems and a spike that lived inside one column has to survive being drawn — but it turns
	// every level into a square wave. The mean is what the number sat at. The legend carries both
	// as figures either way, so the axis never has to be believed on its own.
	// Own draws each line as a percentage of its own peak rather than against the shared value
	// axis.
	//
	// One axis is the default: the metrics on a panel are there because they answer one question
	// together, and how big each is against the others is most of the answer. It is the wrong
	// reading when the magnitudes are decades apart — six operation counters where queries run in
	// the thousands a second and deletes in the tens leaves five lines flat on the floor — so
	// this is the other one. The legend keeps the real figures either way, so nothing is lost by
	// asking for it.
	Own bool
}

// MinPlotRows is the least room a panel's plot is drawn in. Below three rows a line has nowhere to
// be a shape: everything reads as flat, a spike, or both.
const MinPlotRows = 3

// panelIndent is the margin a panel's value axis begins at.
const panelIndent = 2

// panelMarks is how a panel handles two lines in one cell, and it depends on what it has to work
// with. In colour, every line is the same glyph and told apart by colour alone, so a shared cell
// alternates between its owners' colours: still one line, visibly two of them, and nothing on the
// chart to decode. Printed without colour there is nothing to alternate, so the cell becomes a
// mark — ≡ for lines resting at the same value, ┼ for one climbing through another.
func panelMarks(color bool) overlapMarks {
	if color {
		return overlapMarks{interleave: true}
	}
	return overlapMarks{level: '&', cross: 'X'}
}

const (
	// panelFigureColumn is the room a legend entry leaves for its numbers, wide enough for the
	// widest pair the catalogue produces — "999.9M/999.9M". It is fixed rather than measured so
	// that how many lines a legend takes follows from the names and the width alone: a board works
	// out its layout before it draws anything, and cannot afford to bin every panel to find out.
	panelFigureColumn = 13
	// panelLegendGap is the space between two entries.
	panelLegendGap = 3
)

// PanelLines is what a panel costs on a screen: a title, its plot, and however many lines its
// legend needs. A caller laying out a board needs it before anything is drawn, which is why it
// takes the names rather than the series.
func PanelLines(names []string, plotRows, width int) int {
	return 1 + max(MinPlotRows, plotRows) + len(panelLegendRows(names, width))
}

// panelLegendNames are the legend's names, cut to what one line can hold. A wiredTiger name runs to
// sixty characters, and an entry wider than the panel would print past the edge of a terminal that
// has no horizontal scroll. The tail is what is kept: it is the identifying half of a metric name.
func panelLegendNames(names []string, width int) []string {
	room := max(4, width-panelIndent-3-1-panelFigureColumn)
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = name
		if utf8.RuneCountInString(name) > room {
			out[i] = Fit(name, room, AlignTail)
		}
	}
	return out
}

// panelLegendRows is how many entries go on each line of the legend.
//
// The figures are worth wrapping for. A panel of six operation counters cannot fit six names and
// six pairs of numbers across a hundred columns, and dropping the numbers to stay on one line takes
// away the half of the legend that says how big any of this is.
func panelLegendRows(names []string, width int) []int {
	rows := make([]int, 0, 2)
	used, count := panelIndent, 0
	for _, name := range panelLegendNames(names, width) {
		entry := 3 + utf8.RuneCountInString(name) + 1 + panelFigureColumn
		if count > 0 && used+panelLegendGap+entry > width {
			rows = append(rows, count)
			used, count = panelIndent+entry, 1
			continue
		}
		if count > 0 {
			used += panelLegendGap
		}
		used, count = used+entry, count+1
	}
	if count > 0 || len(rows) == 0 {
		rows = append(rows, count)
	}
	return rows
}

// PanelIndent is the column a panel's plot begins in: the margin, the value axis, and its bracket.
// A caller drawing one time axis under a whole board needs it to line the axis up with the plots.
func PanelIndent() int { return panelIndent + MaxAxisGutter + 2 }

// PanelColumns is the plot columns a panel of this width gives its chart.
func PanelColumns(width int) int { return max(8, width-PanelIndent()) }

// PanelBins is how many bins to build for a panel that wide: one per column, so nothing is folded
// away between binning the data and drawing it.
func PanelBins(width int) int { return PanelColumns(width) }

// MetricPanel draws several metrics of one stream on one chart: one value axis, one window, and a
// colour and a glyph set per metric, with a legend under it carrying the figures.
//
// The metrics on a panel are there because they answer one question together — inserts mean little
// without the updates and deletes beside them — so they are drawn against one axis, where the
// proportion between them is readable. Each line gets its own stroke set as well as its own colour,
// so a panel pasted into a ticket without colour still has six distinguishable lines, and where two
// of them cross the cell is marked rather than given to whichever was drawn first.
func MetricPanel(rows []series.Series, options PanelOptions) string {
	paint := painter(options.Color)
	width := max(PanelIndent()+8, options.Width)
	plotRows := max(MinPlotRows, options.Height)

	marker, titleStyle := "  ", plain
	if options.Selected {
		marker, titleStyle = "> ", bold
	}
	if len(rows) == 0 {
		return strings.Join([]string{
			paint(Fit(marker+options.Title, width, AlignLeft), titleStyle),
			"  nothing in this bundle reports it",
		}, "\n")
	}

	colors, pens := lineColors(rows), linePens(rows, options.Color)
	drawn := rows
	if options.Own {
		drawn = mapSeries(drawn, ownPeak)
	}

	lo, hi := drawnRange(drawn)

	first, last := options.First, options.Last
	if !(last > first) {
		first, last = overlaySpan(drawn)
	}
	// Bins are laid on round wall-clock intervals, and the window a reader asked for is not. Left
	// as it was, the last slot of every chart fell past the final bin and was drawn as a gap: every
	// panel on the board ended with a dot claiming the node had stopped reporting.
	if step := binMillis(drawn); step > 0 && last > first {
		first, last = math.Floor(first/step)*step, math.Floor(last/step)*step
	}

	slots := PanelColumns(width)
	grid, owner, met := mergedPlot(drawn, slots, plotRows, lo, hi, first, last, pens, panelMarks(options.Color))
	labels, _ := axisLabels(lo, hi, plotRows, panelUnit(drawn))

	out := make([]string, 0, plotRows+2)
	out = append(out, paint(panelHead(marker+options.Title, panelNote(drawn, lo, hi, options.Own), width), titleStyle))
	margin := strings.Repeat(" ", panelIndent)
	for row, label := range labels {
		out = append(out, margin+Fit(label, MaxAxisGutter, AlignRight)+" |"+
			paintRow(grid[row], owner[row], colors, paint))
	}
	return strings.Join(append(out, panelLegend(rows, pens, colors, width, met, paint)), "\n")
}

// drawnRange is the value axis a panel needs: what will actually appear on it, floored at zero.
//
// What will appear, and not the series' own Range. Range is the raw samples' lowest and highest,
// and the plot draws the peak of every bin — so a single negative reading that no bin's peak ever
// shows still reserved part of the chart for a value that could not be drawn on it, and a cache
// that is never empty was charted against an axis that started below zero.
//
// Zero is the floor, so a height is what a number is worth and the proportion between two lines is
// the proportion between two heights: a cache that never empties is a line near the top of its
// chart rather than a chart that empties. It gives way when what is drawn really
// does go below it.
func drawnRange(rows []series.Series) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, row := range rows {
		for _, bucket := range row.Buckets {
			lo, hi = math.Min(lo, bucket.Max), math.Max(hi, bucket.Max)
		}
	}
	if math.IsInf(lo, 1) {
		return 0, 0
	}
	return math.Min(0, lo), hi
}

// panelHead is the title with the reading pushed to the right of it, when the two fit on one line.
//
// What a column is worth and what the height means are the facts no line can carry itself, and the
// screen used to say neither. A reader who cannot tell that one column is half an hour cannot tell
// what the chart is claiming.
func panelHead(title, note string, width int) string {
	if utf8.RuneCountInString(title)+utf8.RuneCountInString(note)+3 <= width {
		return Fit(title, width-utf8.RuneCountInString(note)-1, AlignLeft) + note
	}
	return Fit(title, width, AlignLeft)
}

// panelUnit is the unit the value axis is in, or none when the panel mixes them.
//
// A panel of one unit — a cache in bytes, a queue in operations — can label its axis in that unit.
// One that mixes them cannot: there is no unit that is right for every tick, and picking the first
// line's would mislabel the rest.
func panelUnit(rows []series.Series) string {
	unit := ""
	for i, row := range rows {
		if i == 0 {
			unit = row.Unit
			continue
		}
		if row.Unit != unit {
			return ""
		}
	}
	return unit
}

// panelNote is how to read the panel: how long a column is, and what the value axis covers.
func panelNote(rows []series.Series, lo, hi float64, own bool) string {
	unit := panelUnit(rows)
	note := Amount(lo, unit) + " - " + Amount(hi, unit)
	if own {
		note = "each to its own peak, %"
	}
	if bin := BinLabel(binSecondsOf(rows)); bin != "" {
		note = bin + "/col  .  " + note
	}
	return note
}

// panelLegend names every line on the panel: its glyph, its colour, what it is called and what it
// reached. It wraps rather than dropping the numbers, because the numbers are the half of the
// legend that says how big any of this is.
func panelLegend(rows []series.Series, pens []strokes, colors []ansi, width int, met string, paint func(string, ansi) string) string {
	names := make([]string, len(rows))
	for i, row := range rows {
		names[i] = nameOf(row)
	}
	shown := panelLegendNames(names, width)
	entries := make([]string, len(rows))
	for i, row := range rows {
		figures := "-"
		if row.Stats != nil {
			figures = Amount(row.Stats.Max, row.Unit)
			if row.Stats.Max != row.Stats.Avg {
				figures += "/" + Amount(row.Stats.Avg, row.Unit)
			}
		}
		// Measured against the full figure column but printed at the width the numbers actually
		// need: a line can only come out shorter than the layout allowed for, never longer, so the
		// row count stays exact and the legend does not sit in a field of padding.
		if utf8.RuneCountInString(figures) > panelFigureColumn {
			figures = Fit(figures, panelFigureColumn, AlignLeft)
		}
		entries[i] = paint(strings.Repeat(string(pens[i].flat), 2)+" "+shown[i]+" "+figures, colors[i])
	}

	gap := strings.Repeat(" ", panelLegendGap)
	lines, at := make([]string, 0, 2), 0
	for _, count := range panelLegendRows(names, width) {
		line := strings.Repeat(" ", panelIndent) + strings.Join(entries[at:min(at+count, len(entries))], gap)
		lines = append(lines, strings.TrimRight(line, " "))
		at += count
	}
	// The marks the plot used, on the end of the last line when there is room for them. They are
	// dropped rather than wrapped: the legend's height is what a board laid itself out from, and a
	// note is not worth being one row wrong about.
	if met != "" {
		explained := strings.Join(strings.Split(met, ""), " ") + " lines meet"
		if met == mixedMark {
			explained = "two colours = lines meet"
		}
		note := paint("   "+explained, dim)
		last := lines[len(lines)-1]
		if plainLen(last)+plainLen(note) <= width {
			lines[len(lines)-1] = last + note
		}
	}
	return strings.Join(lines, "\n")
}

// plainLen is the printed width of a string that may carry styling.
func plainLen(text string) int {
	out, styled := 0, false
	for _, glyph := range text {
		switch {
		case glyph == 0x1b:
			styled = true
		case styled:
			styled = glyph != 'm'
		default:
			out++
		}
	}
	return out
}

// mapSeries is a reading of every series on a panel: the same lines, drawn a different way.
func mapSeries(rows []series.Series, read func(series.Series) series.Series) []series.Series {
	out := make([]series.Series, len(rows))
	for i, row := range rows {
		out[i] = read(row)
	}
	return out
}

// binSecondsOf is the bin these series were built at.
func binSecondsOf(rows []series.Series) int64 {
	for _, row := range rows {
		if row.BucketSeconds > 0 {
			return row.BucketSeconds
		}
	}
	return 0
}

// binMillis is that bin as the grid their buckets start on.
func binMillis(rows []series.Series) float64 { return float64(binSecondsOf(rows)) * 1000 }

// lineColors gives each metric on a panel its own colour, in the order the panel lists them.
func lineColors(rows []series.Series) []ansi {
	colors := make([]ansi, len(rows))
	for i := range rows {
		colors[i] = palette[i%len(palette)]
	}
	if len(rows) == 1 {
		colors[0] = accent
	}
	return colors
}

// linePens is the glyph set each metric is drawn with, which depends on whether there is colour.
//
// In colour every line is the same line: colour is what tells them apart, and six dash patterns on
// one chart is a second alphabet to learn on top of it. Printed without colour the glyph is the
// only channel left, so each line takes a set of its own and a panel pasted into a ticket still has
// six lines that can be told apart.
func linePens(rows []series.Series, color bool) []strokes {
	pens := make([]strokes, len(rows))
	for i := range rows {
		pens[i] = strokeSets[0]
		if !color && len(rows) > 1 {
			pens[i] = strokesFor(i)
		}
	}
	return pens
}

// ownPeak rewrites a series as a percentage of its own range, so a line whose numbers are decades
// below everything else on the panel still has a shape. The series' Stats are left alone: the
// legend goes on printing what it actually reached, so the reading changes the picture and not the
// facts.
func ownPeak(row series.Series) series.Series {
	lo, hi := drawnRange([]series.Series{row})
	scale := func(value float64) float64 {
		if !(hi > lo) {
			return 0
		}
		return (value - lo) / (hi - lo) * 100
	}
	buckets := make([]series.Bucket, len(row.Buckets))
	copy(buckets, row.Buckets)
	for i := range buckets {
		buckets[i].Min, buckets[i].Avg, buckets[i].Max = scale(buckets[i].Min), scale(buckets[i].Avg), scale(buckets[i].Max)
	}
	row.Buckets = buckets
	row.Chart = &series.Scale{Min: 0, Max: 100}
	return row
}

// LineTable is the numbers behind a panel: a row per series, so an open panel says exactly what
// each of its lines reached rather than only where they went.
func LineTable(rows []series.Series, options TableOptions) string {
	if len(rows) == 0 {
		return "nothing to show"
	}
	names := make([]string, len(rows))
	modes := make([]string, len(rows))
	stats := make([][]string, 5)
	for i := range stats {
		stats[i] = make([]string, len(rows))
	}
	peaks := make([]string, len(rows))
	for i, row := range rows {
		names[i] = nameOf(row)
		modes[i] = row.Kind()
		for column, value := range statValues(row.Stats, row.Unit) {
			stats[column][i] = value
		}
		peaks[i] = peakTime(row.Stats)
	}
	return Table([]Column{
		{Title: "Line", Align: AlignTail, Values: names},
		{Title: "Mode", Align: AlignLeft, Values: modes, Drop: 4},
		{Title: "Min", Align: AlignRight, Values: stats[0], Drop: 3},
		{Title: "Avg", Align: AlignRight, Values: stats[1]},
		{Title: "Median", Align: AlignRight, Values: stats[2], Drop: 1},
		{Title: "P95", Align: AlignRight, Values: stats[3], Drop: 5},
		{Title: "Max", Align: AlignRight, Values: stats[4]},
		{Title: "Peak", Align: AlignRight, Values: peaks, Drop: 2},
	}, options)
}
