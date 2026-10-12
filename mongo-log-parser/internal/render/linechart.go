package render

import (
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"mongo-log-parser/internal/series"
)

// axisChart draws one series with its value axis and the time ticks under it, against the value
// range and the window it was given.
//
// Both are parameters rather than the series' own, so that several sources of one metric are drawn
// to a scale and a clock they share. A node that stopped reporting at noon otherwise stretched its
// half of the day across the same row as a node that reported all of it, and the two charts read
// as the same hours when they were not.
func axisChart(row series.Series, width, height int, lo, hi float64, fixedGutter int, first, last float64) []string {
	labels, gutter := axisLabels(lo, hi, height, row.Unit)
	if fixedGutter > gutter {
		gutter = fixedGutter
	}
	if !(last > first) {
		first, last = row.Span()
	}

	plot := plotInto(row, max(8, width-gutter-2), height, lo, hi, first, last)
	out := make([]string, 0, height+2)
	for line, label := range labels {
		out = append(out, Fit(label, gutter, AlignRight)+" |"+plot[line])
	}
	slots := utf8.RuneCountInString(plot[0])
	return append(out,
		strings.Repeat(" ", gutter)+" +"+strings.Repeat("-", slots),
		strings.Repeat(" ", gutter+2)+windowTicks(first, last, row.BucketSeconds, slots))
}

// rulerSteps are the intervals a time axis is allowed to tick on, so a reader is always counting
// in units a person counts in.
var rulerSteps = []time.Duration{
	time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second,
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
	// Past a day, because a fortnight of logs is an ordinary thing to be handed. Without these the
	// widest tick was a day, fourteen of them did not fit, and the axis fell back to a day anyway
	// and hung a label on every one of them.
	48 * time.Hour, 72 * time.Hour, 7 * 24 * time.Hour,
}

// TimeRuler is the axis under a board: a rule with a tick at every round interval and the clock
// reading beneath each one.
//
// Two labels at the ends are not an axis. They say what the window is and leave a reader who has
// found a spike with no way to say when it happened, which is the first thing anyone asks. Ticks on
// round intervals also give the eye something to measure against, so the distance between two
// events is readable instead of estimated.
func TimeRuler(first, last float64, columns int) []string {
	columns = max(8, columns)
	rule := []rune(strings.Repeat("-", columns))
	labels := []rune(strings.Repeat(" ", columns))
	if !(last > first) {
		return []string{"+" + string(rule), string(labels)}
	}

	span := time.Duration(last-first) * time.Millisecond
	// Every tick needs room for its own label and a gap, or the labels collide and the axis reads
	// as one long number.
	step := rulerSteps[len(rulerSteps)-1]
	for _, candidate := range rulerSteps {
		if int(span/candidate) <= columns/9 {
			step = candidate
			break
		}
	}
	// A tick a day apart labelled 15:04 reads 00:00 every time, which is an axis that says nothing:
	// once the ticks are days, the date is the part that tells them apart.
	layout := "15:04"
	switch {
	case step < time.Minute:
		layout = "15:04:05"
	case step >= 24*time.Hour:
		layout = "01-02"
	}

	start := time.UnixMilli(int64(first)).UTC()
	end := time.UnixMilli(int64(last)).UTC()
	at := start.Truncate(step)
	if at.Before(start) {
		at = at.Add(step)
	}
	written := -1
	for ; !at.After(end); at = at.Add(step) {
		column := int(math.Round(float64(at.Sub(start)) / float64(end.Sub(start)) * float64(columns-1)))
		if column < 0 || column >= columns {
			continue
		}
		rule[column] = '+'
		text := []rune(at.Format(layout))
		// Labels are hung from their tick and skipped rather than overlapped: a clipped clock
		// reading is worse than no reading at the tick beside it.
		from := max(0, column-len(text)/2)
		if from+len(text) > columns {
			from = columns - len(text)
		}
		if from <= written || from < 0 {
			continue
		}
		copy(labels[from:], text)
		written = from + len(text)
	}
	return []string{"+" + string(rule), " " + string(labels)}
}

// WindowAxis is the reading at each end of a window a caller already knows, for a screen whose
// charts were all drawn against it and so need the axis once rather than once each.
//
// A window that crosses midnight is dated. A two-day bundle labelled 06:00 → 21:00 reads as one
// morning to one evening, which is the wrong bundle by a factor of two.
func WindowAxis(first, last float64, slots int) string {
	start, end := time.UnixMilli(int64(first)).UTC(), time.UnixMilli(int64(last)).UTC()
	layout := "15:04"
	switch {
	case last-first < 3600*1000:
		layout = "15:04:05"
	case start.YearDay() != end.YearDay():
		layout = "01-02 15:04"
	}
	from, to := start.Format(layout), end.Format(layout)
	return from + strings.Repeat(" ", max(1, slots-len(from)-len(to))) + to
}

// windowTicks is the clock reading at each end of a window, to the precision the bins can tell
// apart.
func windowTicks(first, last float64, seconds int64, slots int) string {
	if !(last > first) {
		return ""
	}
	if seconds <= 0 {
		seconds = 60
	}
	start, end := clockAt(first, seconds), clockAt(last, seconds)
	return start + strings.Repeat(" ", max(1, slots-len(start)-len(end))) + end
}

// axisLabels are the value-axis ticks and the width they need, chosen so the numbers stay readable
// at whatever magnitude the series happens to sit.
func axisLabels(lo, hi float64, height int, unit string) ([]string, int) {
	span := hi - lo
	tick := func(value float64) string {
		// A byte axis is read in its own units at every tick; the plain forms below are for the
		// counts and rates, where a whole number is the most readable thing an axis can say.
		if unitScale(unit) != 0 {
			return Amount(value, unit)
		}
		switch {
		case span >= 100:
			return fixed(value, 0)
		case span >= 10:
			return fixed(value, 1)
		}
		return Compact(value)
	}
	labels := make([]string, height)
	gutter := 0
	for row := range labels {
		labels[row] = tick(axisValue(lo, hi, row, height))
		gutter = max(gutter, utf8.RuneCountInString(labels[row]))
	}
	// A byte count runs to ten digits and steals a third of a narrow terminal, and the gutter it
	// asks for changes with the metric — so stepping from one chart to the next moved the plot
	// sideways. Past a point the short form says the same thing: 3.7G, not 3690000000.
	if gutter > widestAxisLabel {
		gutter = 0
		for row := range labels {
			labels[row] = Amount(axisValue(lo, hi, row, height), unit)
			gutter = max(gutter, utf8.RuneCountInString(labels[row]))
		}
	}
	return labels, gutter
}

// MaxAxisGutter is the most room a value axis will ever ask for: past it the short form is used
// instead. A caller that wants charts to line up across screens can fix its gutter here and know
// no label will overflow it.
const MaxAxisGutter = 7

const widestAxisLabel = MaxAxisGutter

func axisValue(lo, hi float64, row, height int) float64 {
	if hi == lo {
		return lo
	}
	return hi - (hi-lo)*float64(row)/float64(height-1)
}

// timeTicks is the first and last clock reading, pushed to the ends of the plot.
func timeTicks(row series.Series, slots int) string {
	if len(row.Buckets) == 0 {
		return ""
	}
	seconds := row.BucketSeconds
	if seconds == 0 {
		seconds = 60
	}
	first := Clock(row.Buckets[0].Start, seconds)
	last := Clock(row.Buckets[len(row.Buckets)-1].Start, seconds)
	return first + strings.Repeat(" ", max(1, slots-len(first)-len(last))) + last
}

// gapMark is what a bin with no sample is drawn as: a dot on the floor. A node that stopped
// reporting must not look like a node that went quiet.
const gapMark = '.'

// strokes is the set of glyphs one series is drawn with.
//
// Several series on one chart get different sets, because colour is not always there to tell them
// apart: a chart redirected into a file, pasted into a ticket, or read on a terminal someone set to
// one colour keeps its lines distinct only if their shapes differ too.
type strokes struct {
	flat       rune // a level stretch
	stem       rune // the vertical a steep step is filled with
	riseTop    rune // the line arriving from below
	riseBottom rune
	fallTop    rune // the line dropping away
	fallBottom rune
}

// strokeSets are the ways a line can be drawn, in the order they are handed out. Light comes first
// because a chart of a single series has always been light.
//
// There are eight because a panel draws one line per metric and Operations has six: insert, query,
// update, delete, getmore and command. A set that wrapped gave two of them the same glyph, and the
// legend then had two entries a reader could not tell apart without colour.
var strokeSets = []strokes{
	{'-', '|', '+', '+', '+', '+'},
	{'=', '!', '!', '!', '!', '!'},
	{'~', ':', ':', ':', ':', ':'},
	{':', ':', ':', ':', ':', ':'},
	{'*', '*', '*', '*', '*', '*'},
	{'#', '#', '#', '#', '#', '#'},
	{'%', '%', '%', '%', '%', '%'},
	{'@', '@', '@', '@', '@', '@'},
}

// strokesFor is the glyph set the nth series on a chart is drawn with.
func strokesFor(index int) strokes {
	return strokeSets[((index%len(strokeSets))+len(strokeSets))%len(strokeSets)]
}

// plotInto draws the shape of one series against a value range and a time span it may share with
// others, so several sources can be laid over one another and still line up.
func plotInto(row series.Series, width, height int, lo, hi, first, last float64) []string {
	return plotStrokes(row, width, height, lo, hi, first, last, strokeSets[0])
}

// plotStrokes is plotInto with the glyph set named, for a chart that carries more than one series.
func plotStrokes(row series.Series, width, height int, lo, hi, first, last float64, pen strokes) []string {
	values := row.BinnedOver(width, first, last)
	grid := make([][]rune, height)
	for line := range grid {
		grid[line] = []rune(strings.Repeat(" ", len(values)))
	}
	previous := -1
	for x, value := range values {
		if math.IsNaN(value) {
			grid[height-1][x] = gapMark
			previous = -1
			continue
		}
		line := height - 1
		if hi != lo {
			line = int(math.Round((hi - value) / (hi - lo) * float64(height-1)))
		}
		line = min(height-1, max(0, line))
		switch {
		case previous < 0 || previous == line:
			grid[line][x] = pen.flat
		default:
			top, bottom := previous, line
			rising := previous > line
			if rising {
				top, bottom = line, previous
			}
			if rising {
				grid[top][x], grid[bottom][x] = pen.riseTop, pen.riseBottom
			} else {
				grid[top][x], grid[bottom][x] = pen.fallTop, pen.fallBottom
			}
			for y := top + 1; y < bottom; y++ {
				grid[y][x] = pen.stem
			}
		}
		previous = line
	}
	lines := make([]string, height)
	for line := range grid {
		lines[line] = string(grid[line])
	}
	return lines
}

// TimeAxis is the clock reading at each end of a plot, for a caller stacking or tiling several
// charts that cover one window and so need the axis once rather than once each.
//
// The ends are the window every chart was drawn against, not the first series' own. A node that
// stopped reporting at noon still has a panel as wide as the board, and reading the axis off that
// node labelled the whole board with the hours only one of them covered.
func TimeAxis(rows []series.Series, slots int) string {
	first, last := overlaySpan(rows)
	if !(last > first) {
		for _, row := range rows {
			if len(row.Buckets) > 0 {
				return timeTicks(row, slots)
			}
		}
		return ""
	}
	seconds := int64(0)
	for _, row := range rows {
		if row.BucketSeconds > 0 {
			seconds = row.BucketSeconds
			break
		}
	}
	return windowTicks(first, last, seconds, slots)
}

// clockAt is a moment in epoch milliseconds, shortened to the precision a bin of this size can
// actually distinguish.
func clockAt(millis float64, seconds int64) string {
	at := time.UnixMilli(int64(millis)).UTC()
	if seconds%60 == 0 {
		return at.Format("15:04")
	}
	return at.Format("15:04:05")
}
