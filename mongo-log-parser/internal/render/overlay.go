package render

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"mongo-log-parser/internal/series"
)

// OverlayOptions is how much room a chart gets and how it is drawn.
type OverlayOptions struct {
	Width  int
	Height int
	Color  bool
	// Gutter fixes the width of the value axis. Zero lets a chart size its own, which is right on
	// its own and wrong for several stacked together: their plots would not line up, and the eye
	// reads a shifted baseline as a different measurement.
	Gutter int
	// Compact draws the title, the plot and nothing else: no legend, no window line, no per-source
	// numbers, no time ticks. It is for several charts on one screen, where those belong once
	// around the whole screen rather than once per chart.
	Compact bool
}

// palette gives each stream on a chart a colour of its own. Cyan leads because a single-source
// chart has always been cyan; the rest are chosen to stay apart on both dark and light terminals.
var palette = []ansi{
	"\x1b[36m", // cyan
	"\x1b[33m", // yellow
	"\x1b[35m", // magenta
	"\x1b[32m", // green
	"\x1b[34m", // blue
	"\x1b[31m", // red
	// A panel draws one line per metric and Operations alone has six of them, so the palette runs
	// past the six a per-stream chart ever needed. The bright halves are second because they read
	// less well on a light terminal, and nothing should have to use one until it must.
	"\x1b[96m", // bright cyan
	"\x1b[93m", // bright yellow
	"\x1b[95m", // bright magenta
	"\x1b[92m", // bright green
	"\x1b[94m", // bright blue
	"\x1b[91m", // bright red
}

// sourceColors assigns a colour per source in the order the sources appear, so the same stream
// keeps the same colour down a report. One source keeps the plain accent it always had.
func sourceColors(rows []series.Series) map[string]ansi {
	colors := make(map[string]ansi, len(rows))
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		if _, seen := colors[row.Source]; seen {
			continue
		}
		colors[row.Source] = palette[len(order)%len(palette)]
		order = append(order, row.Source)
	}
	if len(order) < 2 {
		for source := range colors {
			colors[source] = accent
		}
	}
	return colors
}

// sourceStrokes gives each stream on a chart a glyph set of its own, in the same order and by the
// same key as its colour, so the legend can name both at once.
func sourceStrokes(rows []series.Series) map[string]strokes {
	pens := make(map[string]strokes, len(rows))
	for _, source := range sourcesOf(rows) {
		pens[source] = strokesFor(len(pens))
	}
	if len(pens) < 2 {
		// One stream has nothing to be told apart from, and the light set reads best on its own.
		for source := range pens {
			pens[source] = strokeSets[0]
		}
	}
	return pens
}

// penPerSource is that mapping resolved down each row of a chart.
func penPerSource(rows []series.Series) []strokes {
	pens := sourceStrokes(rows)
	out := make([]strokes, len(rows))
	for i, row := range rows {
		out[i] = pens[row.Source]
	}
	return out
}

// sourcesOf lists the distinct sources in the order they appear.
func sourcesOf(rows []series.Series) []string {
	seen := map[string]bool{}
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Source == "" || seen[row.Source] {
			continue
		}
		seen[row.Source] = true
		order = append(order, row.Source)
	}
	return order
}

// MetricOverlay draws one metric from several streams on a single chart: one value axis, one time
// axis, and a colour per stream. Use it to see where two nodes stopped agreeing; the stacked form
// in MetricsReport is easier to read one node at a time.
func MetricOverlay(rows []series.Series, options OverlayOptions) string {
	if len(rows) == 0 {
		return "No metrics to show."
	}
	width, color := options.Width, options.Color
	paint := painter(color)
	colors := sourceColors(rows)
	height := max(2, options.Height)

	head := nameOf(rows[0]) + " | " + rows[0].Kind()
	if options.Compact {
		head += " | max " + Amount(peakOf(rows), rows[0].Unit)
	} else {
		head += " | " + plural(len(sourcesOf(rows)), "source")
	}
	out := []string{paint(head, bold)}
	if !options.Compact {
		if line := Sources(rows, color); line != "" {
			out = append(out, line)
		}
	}
	window := overlayWindow(rows)
	if window == "" {
		return strings.Join(append(out, "No samples in this window."), "\n")
	}
	if !options.Compact {
		out = append(out, paint(window, dim))
	}

	lo, hi := overlayScale(rows)
	labels, gutter := axisLabels(lo, hi, height, rows[0].Unit)
	if options.Gutter > gutter {
		gutter = options.Gutter
	}
	slots := max(8, width-gutter-2)
	first, last := overlaySpan(rows)

	// No overlap marks here: an overlay is one metric as several nodes reported it, and the legend
	// order already says which node keeps a cell the others wanted.
	grid, owner, _ := mergedPlot(rows, slots, height, lo, hi, first, last, penPerSource(rows), overlapMarks{})
	byRow := make([]ansi, len(rows))
	for i, row := range rows {
		byRow[i] = colors[row.Source]
	}
	for row, label := range labels {
		out = append(out, Fit(label, gutter, AlignRight)+" ┤"+paintRow(grid[row], owner[row], byRow, paint))
	}
	plotted := utf8.RuneCountInString(string(grid[0]))
	out = append(out, strings.Repeat(" ", gutter)+" └"+strings.Repeat("─", plotted))
	if options.Compact {
		return strings.Join(out, "\n")
	}
	out = append(out, strings.Repeat(" ", gutter+2)+timeTicks(rows[0], plotted))
	// The per-source numbers are the second half of the legend: squeezing the name here would take
	// away the only thing that says which line these numbers belong to.
	name := 0
	for _, row := range rows {
		name = max(name, utf8.RuneCountInString(row.Source))
	}
	for _, row := range rows {
		values := statValues(row.Stats, row.Unit)
		out = append(out, paint(Fit(row.Source, name, AlignLeft), colors[row.Source])+
			"  min "+values[0]+" | avg "+values[1]+" | p95 "+values[3]+" | max "+values[4])
	}
	return strings.Join(out, "\n")
}

// overlapMarks is what a chart does where two lines want the same cell. A cell holds one glyph, so
// somebody has to lose it, and the old answer — the first series drawn keeps it — quietly hid the
// others: a line that disappears behind another reads as a line that stopped.
//
// There are two answers because there are two channels. With colour, the cell keeps its line glyph
// and is painted in each owner's colour by turns, so lines lying on top of each other read as one
// line in two colours: still a line, and visibly two of them. Without colour there is nothing to
// alternate, so the cell is drawn as a mark instead — one for lines resting at the same value,
// which is the whole floor of a counter panel, and one for a line climbing through another.
//
// The zero value asks for neither and the first series keeps the cell, which is right when the
// series are the same metric from different nodes and the legend order already says who wins.
type overlapMarks struct {
	level      rune // several lines at the same height
	cross      rune // one line climbing through another
	interleave bool // paint a shared cell in each owner's colour by turns
}

// marking reports whether a chart wants shared cells called out at all.
func (o overlapMarks) marking() bool { return o.interleave || o.cross != 0 }

// mergedPlot draws every series into one grid and remembers which series owns each cell, so the
// line can be coloured afterwards. It reports back the marks it actually used, for a legend that
// has to explain them.
//
// Two lines that want the same cell are a real problem, because a cell holds one glyph. With marks
// set, the cell is drawn as one of them and belongs to nobody: both lines are still accounted for,
// and the reader can see where they met rather than watching one of them vanish behind the other.
// Two marks and not one because the two cases look nothing alike — six counters resting at zero
// share the whole floor of the chart, and a row of crossing marks along it is louder than the data
// above it, while a level mark reads as the stack of flat lines it is. Without marks the earlier
// series keeps the cell, which is right when the series are the same metric from different nodes
// and the legend order already says who wins. A gap never takes a cell from a line that has data
// there, and a line always takes a cell from a gap.
func mergedPlot(rows []series.Series, slots, height int, lo, hi, first, last float64, pens []strokes, marks overlapMarks) ([][]rune, [][]int, string) {
	// A window with fewer bins than the terminal has columns produces a narrower plot than it was
	// offered. The grid takes that width, not the width asked for, so the axis and the time ticks
	// drawn against it end where the data ends instead of stretching past it.
	plots := make([][]string, len(rows))
	columns := 0
	level := make(map[rune]bool, len(pens))
	for i, row := range rows {
		plots[i] = plotStrokes(row, slots, height, lo, hi, first, last, pens[i])
		level[pens[i].flat] = true
		if len(plots[i]) > 0 {
			columns = max(columns, utf8.RuneCountInString(plots[i][0]))
		}
	}
	if columns == 0 {
		columns = slots
	}

	grid := make([][]rune, height)
	owner := make([][]int, height)
	// shared is who else wanted a cell, allocated only where two lines actually met.
	shared := make([][][]int, height)
	for row := range grid {
		grid[row] = []rune(strings.Repeat(" ", columns))
		owner[row] = make([]int, columns)
		shared[row] = make([][]int, columns)
		for i := range owner[row] {
			owner[row][i] = empty
		}
	}
	used := map[rune]bool{}
	for index, lines := range plots {
		for row := range lines {
			// Ranging a string yields byte offsets, and every glyph a chart draws is multi-byte.
			for column, glyph := range []rune(lines[row]) {
				if glyph == ' ' || column >= columns {
					continue
				}
				held := grid[row][column]
				switch {
				// empty and crossing are both negative owners, and only empty is free: testing
				// for "no series owns this" would hand a crossing back to the next line drawn.
				case owner[row][column] == empty, held == gapMark:
					grid[row][column], owner[row][column] = glyph, index
				case glyph == gapMark || !marks.marking():
					// Nothing to report where another line already drew, and with nothing asked
					// for the earlier series keeps the cell it took.
				case marks.interleave:
					// The glyph stays a line. Who it belongs to is settled below, once every
					// owner of the cell is known.
					if len(shared[row][column]) == 0 {
						shared[row][column] = []int{owner[row][column]}
					}
					shared[row][column] = append(shared[row][column], index)
				default:
					mark := marks.cross
					if level[glyph] && (level[held] || held == marks.level) {
						mark = marks.level
					}
					grid[row][column], owner[row][column] = mark, crossing
					used[mark] = true
				}
			}
		}
	}
	// A cell several lines want goes to each of them by turns, one column each, so a stretch where
	// two lines sit at the same value is drawn as one line alternating between their two colours.
	// The eye reads that as "both of these are here", which is what it is.
	mixed := false
	for row := range shared {
		for column, owners := range shared[row] {
			if len(owners) < 2 {
				continue
			}
			owner[row][column] = owners[column%len(owners)]
			mixed = true
		}
	}
	if mixed {
		return grid, owner, mixedMark
	}
	return grid, owner, markList(marks, used)
}

// mixedMark is what a legend calls the alternating colour, rather than a glyph the chart drew.
const mixedMark = "mixed"

// markList is the marks a plot actually used, in the order they are worth explaining.
func markList(marks overlapMarks, used map[rune]bool) string {
	out := make([]rune, 0, 2)
	for _, mark := range []rune{marks.level, marks.cross} {
		if used[mark] {
			out = append(out, mark)
		}
	}
	return string(out)
}

const (
	// empty is a cell no line has drawn in yet.
	empty = -1
	// crossing is the owner of a cell two lines both wanted: no series, so it is painted in
	// neither's colour and reads as the meeting point it is.
	crossing = -2
)

// paintRow colours each run of cells with the colour of the series that drew them.
func paintRow(runes []rune, owner []int, colors []ansi, paint func(string, ansi) string) string {
	var out strings.Builder
	for start := 0; start < len(runes); {
		end := start
		for end < len(runes) && owner[end] == owner[start] {
			end++
		}
		segment := string(runes[start:end])
		if owner[start] < 0 || owner[start] >= len(colors) {
			out.WriteString(segment)
		} else {
			out.WriteString(paint(segment, colors[owner[start]]))
		}
		start = end
	}
	return out.String()
}

// overlayScale is the value axis every series on the chart shares; without it two nodes drawn to
// their own scales look alike however far apart their numbers are.
func overlayScale(rows []series.Series) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for i := range rows {
		low, high := rows[i].Range()
		lo, hi = math.Min(lo, low), math.Max(hi, high)
	}
	if math.IsInf(lo, 1) {
		return 0, 0
	}
	return lo, hi
}

// overlaySpan is the time axis every series shares, for the same reason.
func overlaySpan(rows []series.Series) (first, last float64) {
	first, last = math.Inf(1), math.Inf(-1)
	for _, row := range rows {
		if len(row.Buckets) == 0 {
			continue
		}
		rowFirst, rowLast := row.Span()
		first, last = math.Min(first, rowFirst), math.Max(last, rowLast)
	}
	if math.IsInf(first, 1) {
		return 0, 0
	}
	return first, last
}

func overlayWindow(rows []series.Series) string {
	for _, row := range rows {
		if len(row.Buckets) == 0 {
			continue
		}
		return row.Buckets[0].Start + " -> " + row.Buckets[len(row.Buckets)-1].Start +
			" | " + BinLabel(row.BucketSeconds) + "/bin | " + binNote([]series.Series{row}) + "; · = gap"
	}
	return ""
}

// peakOf is the largest value any of these series reached, for a compact title that has to say in
// one number what the axis would otherwise spell out.
func peakOf(rows []series.Series) float64 {
	_, hi := overlayScale(rows)
	return hi
}

func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(count) + " " + noun + "s"
}
