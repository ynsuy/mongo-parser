package render

import (
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"mongo-log-parser/internal/series"
)

// BarOptions is how much room a bar chart gets and what it is called.
type BarOptions struct {
	Width int
	// Height is the rows of one strip. Every series is a strip of its own.
	Height int
	// Title puts every series on one panel, each strip on its own scale. Empty groups the series
	// by metric, a panel per metric and a strip per stream on one shared scale.
	Title       string
	First, Last float64 // epoch milliseconds; zero uses the series coverage
}

// barGutter is the room left of the bars for the scale labels, fixed so callers can bin to the
// column count before anything is drawn.
const barGutter = 7

// barLevels are the eighths a cell can be filled to.
var barLevels = []rune(" ▁▂▃▄▅▆▇█")

// BarColumns is how many bars fit across a chart that wide.
func BarColumns(width int) int { return max(8, width-barGutter-2) }

// BarChart draws series as compact vertical bars: one bar per column, eight levels per row, a
// strip per series and a shared UTC time axis under each panel. A blank cell is the floor; a dot
// on the floor is a bin with no samples.
func BarChart(rows []series.Series, options BarOptions) string {
	if len(rows) == 0 {
		return "No metrics to show."
	}
	first, last := options.First, options.Last
	if !(last > first) {
		first, last = seriesSpan(rows)
	}
	height := max(2, options.Height)
	columns := BarColumns(options.Width)

	var panels [][]series.Series
	if options.Title != "" {
		panels = [][]series.Series{rows}
	} else {
		index := map[string]int{}
		for _, row := range rows {
			if i, ok := index[row.Metric]; ok {
				panels[i] = append(panels[i], row)
				continue
			}
			index[row.Metric] = len(panels)
			panels = append(panels, []series.Series{row})
		}
	}

	out := make([]string, 0, len(panels))
	for _, panel := range panels {
		out = append(out, barPanel(panel, options, first, last, columns, height))
	}
	return strings.Join(out, "\n\n")
}

func barPanel(rows []series.Series, options BarOptions, first, last float64, columns, height int) string {
	title := options.Title
	if title == "" {
		title = nameOf(rows[0])
	}
	values := make([][]float64, len(rows))
	slots, gaps := 0, false
	for i := range rows {
		values[i] = rows[i].BinnedOver(columns, first, last)
		slots = max(slots, len(values[i]))
		for _, value := range values[i] {
			gaps = gaps || math.IsNaN(value)
		}
	}
	// Streams of one metric share a scale so nodes compare; different series on a titled panel
	// each get their own, or the small ones lie flat on the floor.
	base, top := barScale(rows)

	note := barNote(rows, first, last, slots)
	if gaps {
		note += " · · = no data"
	}
	lines := []string{}
	single := len(rows) == 1
	if single {
		head := title
		if options.Title == "" && rows[0].Source != "" {
			head += " · " + rows[0].Source
		}
		lines = append(lines, fitLine(head+" · "+barStats(rows[0]), options.Width))
	} else {
		lines = append(lines, fitLine(title, options.Width))
	}
	for i, row := range rows {
		if !single {
			name := nameOf(row)
			if options.Title == "" {
				name = row.Source
			}
			lines = append(lines, fitLine(strings.Repeat(" ", barGutter+2)+name+" · "+barStats(row), options.Width))
		}
		if options.Title != "" {
			base, top = barScale([]series.Series{row})
		}
		lines = append(lines, barStrip(values[i], slots, base, top, height, row.Unit)...)
	}
	axis := barAxis(first, last, slots, rows)
	if len(axis) > 0 {
		axis[len(axis)-1] += " · " + note
	}
	lines = append(lines, axis...)
	return strings.Join(lines, "\n")
}

// barScale is the range every strip of a panel is drawn against. Bars stand on zero unless the
// data goes below it, or moves less than its own distance from zero: then they stand on a round
// floor under the minimum, or a level like 12k↔13k would draw as one solid block.
func barScale(rows []series.Series) (base, top float64) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for i := range rows {
		if len(rows[i].Buckets) == 0 {
			continue
		}
		l, h := rows[i].Range()
		lo, hi = math.Min(lo, l), math.Max(hi, h)
	}
	if math.IsInf(lo, 0) || math.IsNaN(lo) || math.IsInf(hi, 0) || math.IsNaN(hi) {
		return 0, 1
	}
	base = math.Min(0, lo)
	top = math.Max(hi, 0)
	if lo > 0 && hi > lo && hi-lo < lo {
		step := math.Pow(10, math.Floor(math.Log10(hi-lo)))
		base = math.Floor(lo/step) * step
	}
	if !(top > base) {
		top = base + 1
	}
	return base, top
}

func barStrip(values []float64, slots int, base, top float64, height int, unit string) []string {
	cells := make([][]rune, height)
	for r := range cells {
		cells[r] = []rune(strings.Repeat(" ", slots))
	}
	for column, value := range values {
		if math.IsNaN(value) {
			cells[height-1][column] = '·'
			continue
		}
		eighths := int(math.Round((value - base) / (top - base) * float64(height*8)))
		if value > base && eighths == 0 {
			eighths = 1
		}
		eighths = min(eighths, height*8)
		for r := 0; r < height; r++ {
			fill := min(8, max(0, eighths-r*8))
			cells[height-1-r][column] = barLevels[fill]
		}
	}
	lines := make([]string, height)
	for r := range cells {
		// Every row is labelled at its top edge so a short bar reads against the row it sits in.
		label, edge := Amount(base+(top-base)*float64(height-r)/float64(height), unit), " ┤"
		if r == height-1 {
			label = Amount(base, unit)
		}
		lines[r] = strings.TrimRight(Fit(label, barGutter, AlignRight)+edge+string(cells[r]), " ")
	}
	return lines
}

// barStats is the figures a strip is read by, so the bars never have to be believed alone.
func barStats(row series.Series) string {
	stats := row.Stats
	if stats == nil {
		return "no samples"
	}
	parts := []string{"min " + Amount(stats.Min, row.Unit), "avg " + Amount(stats.Avg, row.Unit), "p95 " + Amount(stats.P95, row.Unit)}
	peak := "max " + Amount(stats.Max, row.Unit)
	if at := peakTime(stats); at != "-" {
		peak += " @" + at
	}
	return strings.Join(append(parts, peak), " · ")
}

// barNote says what one bar stands for: how long, and which reduction of that time.
func barNote(rows []series.Series, first, last float64, slots int) string {
	// Bins folded into a column widen the bar by about a whole number of bins.
	seconds := max(1, binSecondsOf(rows))
	if slots > 0 && last > first {
		bins := int64(math.Round((last-first)/1000/float64(seconds))) + 1
		seconds *= max(1, int64(math.Round(float64(bins)/float64(slots))))
	}
	reduction := "peak"
	if rows[0].Low {
		reduction = "trough"
	} else if rows[0].PlotAggregation == "avg" {
		reduction = "mean"
	}
	return "bar = " + BinLabel(seconds) + " " + reduction
}

// barAxis is the time ruler under a panel: a tick at each round time, labelled in UTC.
func barAxis(first, last float64, slots int, rows []series.Series) []string {
	if slots == 0 {
		return nil
	}
	bucket := float64(max(1, binSecondsOf(rows))) * 1000
	span := math.Max(bucket, last-first+bucket)
	steps := []int64{60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400, 172800, 604800}
	step := steps[len(steps)-1]
	for _, candidate := range steps {
		if float64(candidate)*1000/span*float64(slots) >= 8 {
			step = candidate
			break
		}
	}
	ruler := []rune(strings.Repeat("─", slots))
	labels := []rune(strings.Repeat(" ", slots+6))
	stepMillis := float64(step) * 1000
	for tick := math.Ceil(first/stepMillis) * stepMillis; tick < first+span; tick += stepMillis {
		column := int(math.Floor((tick - first) / span * float64(slots)))
		if column < 0 || column >= slots {
			continue
		}
		ruler[column] = '┬'
		moment := time.UnixMilli(int64(tick)).UTC()
		text := moment.Format("15:04")
		if step >= 86400 || (moment.Hour() == 0 && moment.Minute() == 0) {
			text = moment.Format("01-02")
		}
		copy(labels[column:], []rune(text))
	}
	pad := strings.Repeat(" ", barGutter+1)
	return []string{pad + "└" + string(ruler), strings.TrimRight(pad+" "+string(labels), " ") + " UTC"}
}

func fitLine(text string, width int) string {
	if width <= 0 || utf8.RuneCountInString(text) <= width {
		return text
	}
	return Fit(text, width, AlignLeft)
}

// seriesSpan is the first and last bucket start across rows, in epoch milliseconds.
func seriesSpan(rows []series.Series) (first, last float64) {
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

// binSecondsOf is the bin size the rows were built with.
func binSecondsOf(rows []series.Series) int64 {
	for _, row := range rows {
		if row.BucketSeconds > 0 {
			return row.BucketSeconds
		}
	}
	return 0
}
