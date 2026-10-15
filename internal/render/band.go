package render

import (
	"strings"
	"time"
)

// A band is one row of a categorical timeline: a letter per column saying which of a small set of
// states something was in at that moment. It is what a line chart cannot draw — a replica set
// member is PRIMARY or it is not, and there is no value axis between the two.
//
// Everything here is built so that "nothing was watching" is a state of the drawing and not a gap
// in it. A band that renders the absence of evidence the same as the last state seen is the reason
// a set with a primary the whole time was reported as having none.

// BandSpan is one stretch of a row in one state. Spans are given in order and may leave gaps; a gap
// is drawn as unknown.
type BandSpan struct {
	From, To time.Time
	// State is the key into the legend. An empty state is unknown.
	State string
}

// BandRow is one labelled row of a band chart.
type BandRow struct {
	Label string
	Spans []BandSpan
	// Muted dims the whole row. It is for a row the bundle only heard about second hand, so a
	// reader does not read a guess as a measurement.
	Muted bool
}

// BandGroup is a set of rows drawn together under a heading, with the rows of one replica set
// under the name of that set.
type BandGroup struct {
	Title string
	// Lead is drawn above the group's rows, indented with them: the one derived row a reader wants
	// before the members themselves, such as whether the set had anybody to write to.
	Lead *BandRow
	Rows []BandRow
}

// BandKey is how one state is drawn and what to call it under the chart.
type BandKey struct {
	State  string
	Glyph  rune
	Legend string
	Style  ansi
}

// BandOptions is everything a band chart needs that is not the data.
type BandOptions struct {
	Width int
	Color bool
	// From and To are the window every row is drawn against, so two rows of different lengths still
	// line up column for column.
	From, To time.Time
	Keys     []BandKey
	// Unknown is what to draw where no span covers a column. It is required: there is no sensible
	// default for "this is not known", and leaving it blank makes it look like empty time.
	Unknown BandKey
	// Mixed is what to draw where a column is not mostly any one state. At one column per quarter
	// hour a member that flapped four times is not in any one state, and picking one of them to
	// draw is picking which quarter of the truth to show.
	Mixed BandKey
}

// bandDominance is the share of a column one state has to hold to be what the column is drawn as.
//
// Drawing Mixed for any column that held more than one state sounds like the careful choice and is
// not: a restart writes STARTUP2, RECOVERING and SECONDARY inside one second, so a quarter-hour
// column that was SECONDARY for fourteen minutes and fifty-nine seconds came out as "several
// states" — and on the column where a member became PRIMARY for the rest of the window, the one
// glyph a reader is looking for was replaced by a shrug. A column is what it mostly was; Mixed is
// for a column that genuinely has no answer.
const bandDominance = 0.6

// bandLabelWidth is the room the row labels get. Fixed rather than sized to the longest name so
// that two groups drawn one above the other start their columns in the same place; a name longer
// than this is trimmed from the left, which is the end that repeats across a set's members.
const bandLabelWidth = 24

// Band draws the groups as aligned rows of state glyphs, with a shared time ruler and a legend.
func Band(groups []BandGroup, options BandOptions) string {
	if len(groups) == 0 || !options.To.After(options.From) {
		return ""
	}
	columns := max(16, options.Width-bandLabelWidth-3)
	paint := painter(options.Color)

	lines := make([]string, 0, 8)
	for _, group := range groups {
		if group.Title != "" {
			lines = append(lines, paint(group.Title, bold))
		}
		if group.Lead != nil {
			lines = append(lines, bandLine(*group.Lead, columns, options, paint))
		}
		for _, row := range group.Rows {
			lines = append(lines, bandLine(row, columns, options, paint))
		}
	}

	ruler := TimeRuler(float64(options.From.UnixMilli()), float64(options.To.UnixMilli()), columns)
	indent := strings.Repeat(" ", bandLabelWidth+1)
	for _, line := range ruler {
		lines = append(lines, indent+line)
	}
	if legend := bandLegend(groups, options, paint); legend != "" {
		lines = append(lines, "  "+legend)
	}
	return strings.Join(lines, "\n")
}

// bandLine draws one row: its label, then one glyph per column.
func bandLine(row BandRow, columns int, options BandOptions, paint func(string, ansi) string) string {
	style := plain
	if row.Muted {
		style = dim
	}
	cells := bandCells(row.Spans, columns, options)
	out := make([]string, 0, columns+1)
	out = append(out, paint(Fit(row.Label, bandLabelWidth, AlignTail), style))
	out = append(out, " ")
	for _, cell := range cells {
		glyph := string(cell.Glyph)
		if row.Muted {
			out = append(out, paint(glyph, dim))
			continue
		}
		out = append(out, paint(glyph, cell.Style))
	}
	return strings.Join(out, "")
}

// bandCells resolves the spans of one row into one key per column.
//
// A column is a stretch of time, not an instant, so the question is what to draw for a column that
// several spans overlap. The rule is: if the states inside it differ, draw Mixed; otherwise draw
// the one state. Which is to say a column never claims a state the row was not in for the whole of
// it, and a flap is visible as a flap rather than flattened to whichever side of it the sampling
// happened to land on.
func bandCells(spans []BandSpan, columns int, options BandOptions) []BandKey {
	keys := make(map[string]BandKey, len(options.Keys))
	for _, key := range options.Keys {
		keys[key.State] = key
	}
	total := options.To.Sub(options.From)
	cells := make([]BandKey, columns)
	// held is how long each state held each column. Time rather than presence: a column is drawn as
	// what it mostly was, and the two questions have different answers wherever a restart or an
	// election happens inside one.
	held := make([]map[string]time.Duration, columns)
	edge := func(column int) (time.Time, time.Time) {
		start := options.From.Add(time.Duration(float64(total) * float64(column) / float64(columns)))
		end := options.From.Add(time.Duration(float64(total) * float64(column+1) / float64(columns)))
		return start, end
	}

	for _, span := range spans {
		from, to := span.From, span.To
		if from.Before(options.From) {
			from = options.From
		}
		if to.After(options.To) {
			to = options.To
		}
		if !to.After(from) {
			continue
		}
		first := int(float64(from.Sub(options.From)) / float64(total) * float64(columns))
		// The end is exclusive, so a span ending exactly on a boundary does not colour the column
		// after it — but a span shorter than one column still has to colour the one it is in, or a
		// thirty-second stretch in PRIMARY disappears from a chart of a day.
		last := int(float64(to.Sub(options.From)-1) / float64(total) * float64(columns))
		first, last = max(0, min(first, columns-1)), max(0, min(last, columns-1))
		for column := first; column <= last; column++ {
			start, end := edge(column)
			if start.Before(from) {
				start = from
			}
			if end.After(to) {
				end = to
			}
			overlap := end.Sub(start)
			// A span shorter than the column it sits in still counts for something, or a state that
			// lasted a second would tie with one that never happened.
			if overlap <= 0 {
				overlap = time.Nanosecond
			}
			if held[column] == nil {
				held[column] = map[string]time.Duration{}
			}
			held[column][span.State] += overlap
		}
	}

	for column := range cells {
		states := held[column]
		if len(states) == 0 {
			cells[column] = options.Unknown
			continue
		}
		leader, longest, sum := "", time.Duration(0), time.Duration(0)
		for state, duration := range states {
			sum += duration
			// Ties go to the state that sorts first, so the same data always draws the same way.
			if duration > longest || (duration == longest && state < leader) {
				leader, longest = state, duration
			}
		}
		if len(states) > 1 && float64(longest) < bandDominance*float64(sum) {
			cells[column] = options.Mixed
			continue
		}
		key, known := keys[leader]
		if !known {
			key = options.Unknown
			if leader != "" {
				// A state nothing declared a glyph for still has to draw as itself rather than as
				// "unknown", which would be a lie about the evidence.
				key = BandKey{State: leader, Glyph: firstRune(leader), Legend: leader}
			}
		}
		cells[column] = key
	}
	return cells
}

// bandLegend names every glyph that was actually drawn, and no others. A legend listing states the
// chart does not contain is as much noise as a chart with no legend at all.
func bandLegend(groups []BandGroup, options BandOptions, paint func(string, ansi) string) string {
	used := map[string]bool{}
	for _, group := range groups {
		rows := group.Rows
		if group.Lead != nil {
			rows = append([]BandRow{*group.Lead}, rows...)
		}
		for _, row := range rows {
			for _, span := range row.Spans {
				used[span.State] = true
			}
		}
	}
	parts := make([]string, 0, len(options.Keys)+2)
	for _, key := range options.Keys {
		// An empty legend is a state that shares its glyph and its name with one already listed.
		if used[key.State] && key.Legend != "" {
			parts = append(parts, paint(string(key.Glyph), key.Style)+" "+key.Legend)
		}
	}
	parts = append(parts,
		paint(string(options.Unknown.Glyph), options.Unknown.Style)+" "+options.Unknown.Legend,
		paint(string(options.Mixed.Glyph), options.Mixed.Style)+" "+options.Mixed.Legend)
	return strings.Join(parts, "   ")
}

func firstRune(text string) rune {
	for _, r := range text {
		return r
	}
	return '?'
}
