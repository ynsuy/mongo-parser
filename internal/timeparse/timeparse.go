// Package timeparse reads the times a person or an agent types: a full timestamp, a date and a
// clock, a month-day and a clock, or a clock alone. Everything is UTC, because that is what the
// logs are in; a short form takes its missing parts from the bundle, and refuses to guess when the
// bundle spans more than one day.
package timeparse

import (
	"fmt"
	"strings"
	"time"
)

var full = []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000", "2006-01-02T15:04:05", "2006-01-02T15:04",
	"2006-01-02 15:04:05.000", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"}
var monthDay = []string{"01-02 15:04:05", "01-02 15:04", "01-02T15:04:05", "01-02T15:04"}
var clock = []string{"15:04:05.000", "15:04:05", "15:04"}

// Parse reads text against a bundle spanning first..last.
func Parse(text string, first, last time.Time) (time.Time, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimSuffix(strings.TrimSuffix(text, " UTC"), "Z")
	if strings.Contains(text, "T") && len(text) > 16 {
		text += "Z"
		if t, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return t.UTC(), nil
		}
		text = strings.TrimSuffix(text, "Z")
	}
	for _, layout := range full {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UTC(), nil
		}
	}
	for _, layout := range monthDay {
		if t, err := time.Parse(layout, text); err == nil {
			year := first.Year()
			if first.IsZero() {
				year = time.Now().UTC().Year()
			}
			candidate := time.Date(year, t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
			if !last.IsZero() && last.Year() != year && candidate.Before(first) {
				candidate = candidate.AddDate(1, 0, 0)
			}
			return candidate, nil
		}
	}
	for _, layout := range clock {
		if t, err := time.Parse(layout, text); err == nil {
			if first.IsZero() {
				return time.Time{}, fmt.Errorf("%q has no date and the bundle has no times to take one from", text)
			}
			if days := dates(first, last); len(days) > 1 {
				return time.Time{}, fmt.Errorf("%q has no date and the bundle covers %s to %s; say which day, e.g. %q",
					text, days[0], days[len(days)-1], days[len(days)-1][5:]+" "+text)
			}
			return time.Date(first.Year(), first.Month(), first.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read %q as a time; use e.g. 2026-01-15T10:00:00Z, \"2026-01-15 10:00\", \"01-15 10:00\" or 10:00", text)
}

func dates(first, last time.Time) []string {
	var out []string
	for d := first.Truncate(24 * time.Hour); !d.After(last); d = d.Add(24 * time.Hour) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out
}

// Window turns the flags a command takes into a range: --from/--to, or --around with --window
// (the width of the whole range, centred on the moment).
func Window(from, to, around, window string, first, last time.Time) (time.Time, time.Time, error) {
	if around != "" {
		if from != "" || to != "" {
			return time.Time{}, time.Time{}, fmt.Errorf("--around replaces --from and --to; give one or the other")
		}
		centre, err := Parse(around, first, last)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		width := 30 * time.Minute
		if window != "" {
			if width, err = time.ParseDuration(window); err != nil {
				return time.Time{}, time.Time{}, fmt.Errorf("--window: %v", err)
			}
		}
		return centre.Add(-width / 2), centre.Add(width / 2), nil
	}
	start, end := time.Time{}, time.Time{}
	var err error
	if from != "" {
		if start, err = Parse(from, first, last); err != nil {
			return start, end, err
		}
	}
	if to != "" {
		if end, err = Parse(to, first, last); err != nil {
			return start, end, err
		}
	}
	if !start.IsZero() && !end.IsZero() && end.Before(start) {
		return start, end, fmt.Errorf("--to %s is before --from %s", end.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	return start, end, nil
}
