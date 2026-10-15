package render

import (
	"math"
	"strings"
	"time"
)

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
