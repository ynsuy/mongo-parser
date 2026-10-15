// Package render turns metric series into terminal tables and charts.
// The layout rules here are a port of dbxray's .pi/lib/chart.ts and must keep its output identical.
package render

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ynsuy/mongo-parser/internal/series"
)

// Duration prints a span the way a reader says it out loud, and rounds to whatever precision is
// worth reading at that size: milliseconds for something under a second, minutes for something over
// an hour. A span that was never measured prints as a hyphen rather than as 0s, which would claim
// the thing took no time at all.
func Duration(span time.Duration) string {
	switch {
	case span <= 0:
		return "-"
	case span < time.Second:
		return span.Round(time.Millisecond).String()
	case span < time.Minute:
		return span.Round(100 * time.Millisecond).String()
	case span < time.Hour:
		return span.Round(time.Second).String()
	}
	// Go spells a whole number of minutes with the seconds still on it, so a ten-hour outage reads
	// "10h15m0s" — three characters of nothing, in a column where every row carries them.
	return strings.TrimSuffix(span.Round(time.Minute).String(), "0s")
}

// Align selects how a cell is padded, and how it is cut when it does not fit.
type Align int

const (
	AlignLeft Align = iota
	AlignRight
	// AlignTail keeps the end of the text, which is the identifying part of a metric path.
	AlignTail
)

// Compact keeps column widths honest: magnitudes get a suffix instead of full precision.
func Compact(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "-"
	}
	abs := math.Abs(value)
	switch {
	case abs == 0:
		return "0"
	case abs >= 1e9:
		return fixed(value/1e9, 1) + "G"
	case abs >= 1e6:
		return fixed(value/1e6, 1) + "M"
	case abs >= 1e3:
		return fixed(value/1e3, 1) + "k"
	case abs >= 100:
		return fixed(value, 0)
	case abs >= 10:
		return fixed(value, 1)
	case abs >= 1:
		return fixed(value, 2)
	case abs >= 0.01:
		return fixed(value, 3)
	case abs >= 0.0001:
		return fixed(value, 4)
	}
	// Scientific notation is precise and unreadable at a glance; a rate this small is just "almost
	// nothing", and the exact figure is one drill-down away.
	return "~0"
}

// binaryPrefixes are the powers of 1024 a byte figure is printed in, and Gi is GiB. The short form
// is deliberate: it is two characters where the current decimal suffix is one, which keeps every
// axis gutter, legend and table column the width it already was, and nobody who reads GiB will
// misread Gi.
var binaryPrefixes = []struct {
	at     float64
	suffix string
}{
	{1 << 50, "Pi"}, {1 << 40, "Ti"}, {1 << 30, "Gi"}, {1 << 20, "Mi"}, {1 << 10, "Ki"},
}

// unitScale is how many bytes one of a unit's own units is worth.
func unitScale(unit string) float64 {
	switch unit {
	case series.UnitBytes:
		return 1
	case series.UnitKiB:
		return 1 << 10
	case series.UnitMiB:
		return 1 << 20
	}
	return 0
}

// Amount prints a value the way its own unit is read.
//
// Bytes are counted in powers of two everywhere a DBA looks — MongoDB's cacheSizeGB, Atlas,
// Grafana, mongostat — so a cache of 68.72 billion bytes is 64 GiB to all of them, and printing
// 68.7G invites the reader to conclude the two tools disagree about a number they agree about.
// Everything that is not bytes stays decimal, because a thousand operations a second is a thousand.
func Amount(value float64, unit string) string {
	scale := unitScale(unit)
	if scale == 0 {
		return Compact(value)
	}
	bytes := value * scale
	if math.IsNaN(bytes) || math.IsInf(bytes, 0) {
		return "-"
	}
	abs := math.Abs(bytes)
	if abs < 1<<10 {
		// Under a kibibyte there is no prefix to use and a "B" on the end buys nothing: these are
		// the axis floor and the counter that never moved.
		return Compact(bytes)
	}
	for _, prefix := range binaryPrefixes {
		if abs >= prefix.at {
			return fixed(bytes/prefix.at, 1) + prefix.suffix
		}
	}
	return Compact(bytes)
}

// fixed matches JavaScript's Number.prototype.toFixed, which rounds halves away from zero.
func fixed(value float64, digits int) string {
	scale := math.Pow(10, float64(digits))
	rounded := math.Round(math.Abs(value)*scale) / scale
	if math.Signbit(value) {
		rounded = -rounded
	}
	return strconv.FormatFloat(rounded, 'f', digits, 64)
}

// Fit pads text to width, or cuts it with an ellipsis when it is too long.
func Fit(text string, width int, align Align) string {
	length := utf8.RuneCountInString(text)
	if length <= width {
		padding := strings.Repeat(" ", width-length)
		if align == AlignRight {
			return padding + text
		}
		return text + padding
	}
	runes := []rune(text)
	if align == AlignTail {
		return "~" + string(runes[length-(width-1):])
	}
	return string(runes[:width-1]) + "~"
}

// BinLabel names a bin size the way a human would say it.
func BinLabel(seconds int64) string {
	switch {
	case seconds <= 0:
		return ""
	case seconds%86400 == 0:
		return fmt.Sprintf("%dd", seconds/86400)
	case seconds%3600 == 0:
		return fmt.Sprintf("%dh", seconds/3600)
	case seconds > 3600 && seconds%60 == 0:
		return fmt.Sprintf("%dh%dm", seconds/3600, seconds%3600/60)
	case seconds%60 == 0:
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%ds", seconds)
}

var peakClock = regexp.MustCompile(`T([\d:]{8})`)

func peakTime(stats *series.Stats) string {
	if stats == nil || stats.Peak == nil {
		return "-"
	}
	if match := peakClock.FindStringSubmatch(stats.Peak.Timestamp); match != nil {
		return match[1]
	}
	return "-"
}

// Wrap folds text onto lines of at most width characters, indenting every line after the first by
// indent spaces. A word longer than the line is cut rather than left to overflow, which is what a
// path or a base64 signature in a log attribute usually is.
func Wrap(text string, width, indent int) []string {
	width = max(width, indent+8)
	room := width - indent
	pad := strings.Repeat(" ", indent)
	lines := make([]string, 0, 4)
	line := ""
	flush := func() {
		if line == "" {
			return
		}
		if len(lines) == 0 {
			lines = append(lines, line)
		} else {
			lines = append(lines, pad+line)
		}
		line = ""
	}
	for _, word := range strings.Fields(text) {
		for utf8.RuneCountInString(word) > room {
			flush()
			runes := []rune(word)
			lines = append(lines, pad+string(runes[:room]))
			word = string(runes[room:])
		}
		switch {
		case line == "":
			line = word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= room:
			line += " " + word
		default:
			flush()
			line = word
		}
	}
	flush()
	return lines
}
