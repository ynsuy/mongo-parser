package render

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mongo-log-parser/internal/series"
)

// sampleSeries builds one source's view of a metric: count values, one per minute, starting at offset
// minutes past a fixed hour.
func sampleSeries(source, metric string, offset int, values ...float64) series.Series {
	base := time.Date(2026, 9, 6, 20, 0, 0, 0, time.UTC)
	buckets := make([]series.Bucket, len(values))
	low, high := values[0], values[0]
	for i, value := range values {
		buckets[i] = series.Bucket{
			Start:   base.Add(time.Duration(offset+i) * time.Minute).Format(time.RFC3339),
			Samples: 1, Min: value, Avg: value, Max: value,
		}
		low, high = min(low, value), max(high, value)
	}
	return series.Series{
		Source: source, Metric: metric, Mode: "gauge", SampleCount: len(values), BucketSeconds: 60,
		Stats:   &series.Stats{Min: low, Avg: values[0], Median: values[0], P95: high, Max: high},
		Buckets: buckets,
	}
}

var escapes = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Two nodes drawn to their own scales look alike however far apart their numbers are. One metric
// gets one value axis, whichever node reported it.
func TestSharedScaleAcrossSourcesOfOneMetric(t *testing.T) {
	rows := []series.Series{
		sampleSeries("node-a", "serverStatus/x", 0, 0, 100, 50, 0),
		sampleSeries("node-b", "serverStatus/x", 0, 0, 4, 2, 0),
	}
	report := MetricsReport(rows, ReportOptions{Width: 100, ChartHeight: 4})
	if strings.Count(report, "100") < 2 {
		t.Errorf("both charts should carry the shared maximum on their axis:\n%s", report)
	}
	// node-b's own maximum must not become an axis label of its own.
	for _, line := range strings.Split(report, "\n") {
		if regexp.MustCompile(`^\s*4(?:\.0)? \|`).MatchString(line) {
			t.Errorf("node-b was drawn to its own scale: %q", line)
		}
	}
}

// Two metrics are different quantities and must keep their own axes.
func TestScaleIsNotSharedBetweenDifferentMetrics(t *testing.T) {
	rows := []series.Series{
		sampleSeries("node-a", "serverStatus/big", 0, 0, 1000),
		sampleSeries("node-a", "serverStatus/small", 0, 0, 3),
	}
	scales := sharedScales(rows)
	if got := scales["serverStatus/big"]; got[1] != 1000 {
		t.Errorf("big metric scale = %v, want max 1000", got)
	}
	if got := scales["serverStatus/small"]; got[1] != 3 {
		t.Errorf("small metric scale = %v, want max 3; a shared axis would flatten it", got)
	}
}

// Colour is decoration on the overlay too: it must not move a single character.
func TestOverlayColorLeavesLayoutAlone(t *testing.T) {
	rows := []series.Series{
		sampleSeries("node-a", "serverStatus/x", 0, 1, 9, 3, 7, 2),
		sampleSeries("node-b", "serverStatus/x", 0, 8, 2, 6, 1, 9),
		sampleSeries("node-c", "serverStatus/x", 0, 4, 4, 4, 4, 4),
	}
	plain := MetricOverlay(rows, OverlayOptions{Width: 100, Height: 6, Color: false})
	colored := MetricOverlay(rows, OverlayOptions{Width: 100, Height: 6, Color: true})
	if plain == colored {
		t.Fatal("colour was requested but nothing changed")
	}
	if stripped := escapes.ReplaceAllString(colored, ""); stripped != plain {
		t.Errorf("colour changed the layout\n--- stripped ---\n%s\n--- plain ---\n%s", stripped, plain)
	}
}

// Every source gets a colour of its own, and the legend says which is which. Three lines with no
// key is three lines nobody can read.
func TestOverlayLegendNamesEverySource(t *testing.T) {
	rows := []series.Series{
		sampleSeries("node-a", "serverStatus/x", 0, 1, 2),
		sampleSeries("node-b", "serverStatus/x", 0, 3, 4),
		sampleSeries("node-c", "serverStatus/x", 0, 5, 6),
	}
	colors := sourceColors(rows)
	seen := map[ansi]string{}
	for _, row := range rows {
		if other, clash := seen[colors[row.Source]]; clash {
			t.Errorf("%s and %s share a colour", other, row.Source)
		}
		seen[colors[row.Source]] = row.Source
	}
	chart := MetricOverlay(rows, OverlayOptions{Width: 100, Height: 6, Color: false})
	legend := strings.Split(chart, "\n")[1]
	for _, row := range rows {
		if !strings.Contains(legend, row.Source) {
			t.Errorf("legend %q does not name %s", legend, row.Source)
		}
	}
}

// A single source keeps the accent it always had rather than becoming "the first of a palette",
// and is still named: the reader should not have to count lines to learn whose data this is.
func TestOneSourceKeepsTheAccentColourAndIsNamed(t *testing.T) {
	rows := []series.Series{sampleSeries("node-a", "serverStatus/x", 0, 1, 2)}
	if got := sourceColors(rows)["node-a"]; got != accent {
		t.Errorf("single source colour = %q, want the accent", got)
	}
	if got := Sources(rows, false); !strings.Contains(got, "node-a") {
		t.Errorf("the one source is not named: %q", got)
	}
}

// The key above a chart names every stream drawn on it, whatever the command showing it.
func TestSourcesNamesEveryStream(t *testing.T) {
	rows := []series.Series{
		sampleSeries("node-a", "serverStatus/x", 0, 1, 2),
		sampleSeries("node-b", "serverStatus/x", 0, 3, 4),
		sampleSeries("node-c", "serverStatus/x", 0, 5, 6),
	}
	key := Sources(rows, false)
	for _, row := range rows {
		if !strings.Contains(key, row.Source) {
			t.Errorf("key %q does not name %s", key, row.Source)
		}
	}
}

// A path is shown by the two segments that identify it, joined by a dot.
func TestMetricNameKeepsTheIdentifyingSegments(t *testing.T) {
	for path, want := range map[string]string{
		"serverStatus/opcounters/query":                            "opcounters.query",
		"serverStatus/wiredTiger/concurrentTransactions/write/out": "write.out",
		"serverStatus/globalLock/currentQueue/total":               "currentQueue.total",
		"systemMetrics/cpu/iowait_ms":                              "cpu.iowait_ms",
		"single":                                                   "single",
	} {
		if got := MetricName(path); got != want {
			t.Errorf("MetricName(%q) = %q, want %q", path, got, want)
		}
	}
}

// A node whose samples start late must be drawn late. Binning each series to its own span would
// slide its line to the left and invent an agreement that never happened.
func TestOverlayPlacesSourcesOnOneTimeSpan(t *testing.T) {
	rows := []series.Series{
		sampleSeries("node-a", "serverStatus/x", 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 9),
		sampleSeries("node-b", "serverStatus/x", 8, 9, 9),
	}
	first, last := overlaySpan(rows)
	lo, hi := overlayScale(rows)
	grid, owner, _ := mergedPlot(rows, 40, 5, lo, hi, first, last, penPerSource(rows), overlapMarks{})

	columns := len(grid[0])
	earliest := columns
	for row := range owner {
		for column, index := range owner[row] {
			if index == 1 && column < earliest {
				earliest = column
			}
		}
	}
	// node-b's samples begin eight minutes into an eleven-minute window.
	if earliest*2 < columns {
		t.Errorf("the late source was drawn from column %d of %d; it starts past the middle of the window",
			earliest, columns)
	}
}

// The chart is drawn from multi-byte glyphs. Merging the sources by byte offset overruns the row.
func TestOverlayRowsAreExactlyAsWideAsTheyLook(t *testing.T) {
	rows := []series.Series{
		sampleSeries("node-a", "serverStatus/x", 0, 1, 9, 2, 8, 3, 7, 4, 6, 5),
		sampleSeries("node-b", "serverStatus/x", 0, 9, 1, 8, 2, 7, 3, 6, 4, 5),
	}
	chart := MetricOverlay(rows, OverlayOptions{Width: 96, Height: 6, Color: false})
	widths := map[int]bool{}
	for _, line := range strings.Split(chart, "\n") {
		if regexp.MustCompile(`^\s*-?\d+(?:\.\d+)?(?:[A-Za-z/]+)? \|`).MatchString(line) {
			widths[utf8.RuneCountInString(line)] = true
		}
	}
	if len(widths) != 1 {
		t.Errorf("plot rows came out at different widths: %v\n%s", widths, chart)
	}
}
