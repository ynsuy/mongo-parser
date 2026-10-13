package render

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mongo-log-parser/internal/series"
)

// A time axis of two end labels is not an axis: a reader who has found a spike still cannot say
// when it happened. The ruler ticks on round intervals and hangs a clock reading from each.
func TestTimeRulerTicksOnRoundIntervals(t *testing.T) {
	start := time.Date(2026, 9, 3, 3, 14, 0, 0, time.UTC)
	lines := TimeRuler(float64(start.UnixMilli()), float64(start.Add(20*time.Hour).UnixMilli()), 120)
	if len(lines) != 2 {
		t.Fatalf("ruler drew %d lines, want a rule and its labels", len(lines))
	}
	if !strings.Contains(lines[0], "+") {
		t.Errorf("the rule has no ticks: %q", lines[0])
	}
	// Twenty hours over a hundred and twenty columns wants a tick every three hours or so, and
	// every label has to be a whole clock reading rather than a clipped one.
	fields := strings.Fields(lines[1])
	if len(fields) < 4 {
		t.Errorf("only %d labels on a 20h axis: %q", len(fields), lines[1])
	}
	for _, field := range fields {
		if len(field) != 5 || field[2] != ':' {
			t.Errorf("label %q is not a whole clock reading", field)
		}
	}
	if utf8.RuneCountInString(lines[0]) != 121 || utf8.RuneCountInString(lines[1]) != 121 {
		t.Errorf("ruler lines are %d and %d runes, want 121 for 120 columns",
			utf8.RuneCountInString(lines[0]), utf8.RuneCountInString(lines[1]))
	}
}

// A fortnight of logs is an ordinary window, and its ticks are days apart. Labelled as a clock
// reading every one of them says 00:00, which is an axis that tells the reader nothing at all.
func TestRulerDatesItsTicksOnceTheyAreDaysApart(t *testing.T) {
	start := time.Date(2026, 8, 29, 5, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		span time.Duration
		want string
	}{
		{6 * time.Hour, "05:30"},
		{3 * 24 * time.Hour, "18:00"},
		{14 * 24 * time.Hour, "09-02"},
		{60 * 24 * time.Hour, "09-07"},
	} {
		lines := TimeRuler(float64(start.UnixMilli()), float64(start.Add(c.span).UnixMilli()), 108)
		if !strings.Contains(lines[1], c.want) {
			t.Errorf("a %s axis is labelled %q, want it to carry %q", c.span, lines[1], c.want)
		}
		// A clock reading may come round again on a long axis — the sequence still reads — but an
		// axis whose every label is the same one cannot say when anything happened.
		labels := strings.Fields(lines[1])
		distinct := map[string]bool{}
		for _, label := range labels {
			distinct[label] = true
		}
		if len(labels) > 1 && len(distinct) == 1 {
			t.Errorf("every tick on a %s axis is labelled the same: %q", c.span, lines[1])
		}
	}
}

func TestCompactFormatting(t *testing.T) {
	for _, c := range []struct {
		value float64
		want  string
	}{
		{0, "0"}, {0.0012, "0.0012"}, {0.05, "0.050"}, {1.5, "1.50"}, {42.7, "42.7"},
		{512, "512"}, {1234, "1.2k"}, {2.5e6, "2.5M"}, {3.1e9, "3.1G"}, {-7.25, "-7.25"},
		{0.0071, "0.0071"}, {0.00001, "~0"},
	} {
		if got := Compact(c.value); got != c.want {
			t.Errorf("Compact(%v) = %q, want %q", c.value, got, c.want)
		}
	}
}

// Bytes are counted in powers of two everywhere else a DBA looks — MongoDB's own cacheSizeGB,
// Atlas, Grafana, mongostat — so a cache of 68.72 billion bytes has to read 64.0 GiB here too.
// Printing 68.7G invites the reader to conclude the two tools disagree about a number they agree
// about. Counts and rates stay decimal, because a thousand operations a second is a thousand.
func TestBytesArePrintedInTheUnitsEveryOtherToolUses(t *testing.T) {
	for _, c := range []struct {
		value float64
		unit  string
		want  string
	}{
		{68719476736, series.UnitBytes, "64.0Gi"},
		{51539607552, series.UnitBytes, "48.0Gi"},
		{1 << 30, series.UnitBytes, "1.0Gi"},
		{1536, series.UnitBytes, "1.5Ki"},
		{512, series.UnitBytes, "512"}, // no prefix below a kibibyte, and no bare "B"
		{0, series.UnitBytes, "0"},
		{-2147483648, series.UnitBytes, "-2.0Gi"},
		// Metrics that arrive already scaled: the value is left as the server reported it.
		{65536000, series.UnitKiB, "62.5Gi"},
		{2048, series.UnitMiB, "2.0Gi"},
		// Everything else is decimal, and unchanged.
		{2400, "", "2.4k"},
		{68719476736, "", "68.7G"},
	} {
		if got := Amount(c.value, c.unit); got != c.want {
			t.Errorf("Amount(%v, %q) = %q, want %q", c.value, c.unit, got, c.want)
		}
	}
}

// A table cuts the column that has room to lose, not the one that happens to be first. A Severity
// column cut to "criti~" loses everything it says; the sentence beside it can spare the characters.
func TestElasticColumnGivesUpTheWidth(t *testing.T) {
	long := strings.Repeat("x", 200)
	columns := []Column{
		{Title: "Severity", Values: []string{"critical"}},
		{Title: "Finding", Values: []string{long}, Elastic: true},
	}
	out := Table(columns, TableOptions{Width: 60})
	if !strings.Contains(out, "critical") {
		t.Errorf("the fixed column was squeezed:\n%s", out)
	}
	if strings.Contains(out, long) {
		t.Error("the elastic column was not cut, so the table overflows")
	}

	// Without the flag nothing is cut: the table is printed at its natural width.
	columns[1].Elastic = false
	if out := Table(columns, TableOptions{Width: 60}); !strings.Contains(out, "critical") || !strings.Contains(out, long) {
		t.Errorf("a column without the flag was cut:\n%s", out)
	}
}

// A narrow terminal never costs a column: every column given is printed, header and values.
func TestTableKeepsEveryColumn(t *testing.T) {
	var columns []Column
	for i := range 12 {
		columns = append(columns, Column{Title: fmt.Sprintf("Col%02d", i), Values: []string{fmt.Sprintf("value-%02d", i)}})
	}
	out := Table(columns, TableOptions{Width: 40})
	for _, column := range columns {
		if !strings.Contains(out, column.Title) || !strings.Contains(out, column.Values[0]) {
			t.Errorf("%s is missing from a 40-wide table:\n%s", column.Title, out)
		}
	}
}

// TestBandDrawsWhatAColumnMostlyWas: a column is a stretch of time, not an instant, and a restart
// writes three states inside one second. A column that was SECONDARY for fourteen of its fifteen
// minutes is a SECONDARY column; only one with no majority has no answer.
func TestBandDrawsWhatAColumnMostlyWas(t *testing.T) {
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	options := BandOptions{
		Width: 20 + 24 + 3, Color: false, From: start, To: start.Add(20 * time.Minute),
		Keys: []BandKey{
			{State: "PRIMARY", Glyph: 'P', Legend: "PRIMARY"},
			{State: "SECONDARY", Glyph: 'S', Legend: "SECONDARY"},
			{State: "RECOVERING", Glyph: 'r', Legend: "RECOVERING"},
		},
		Unknown: BandKey{Glyph: '.', Legend: "unknown"},
		Mixed:   BandKey{Glyph: 'x', Legend: "mixed"},
	}
	// Twenty columns of a minute each. Minute 0-4 unknown, 5 a restart that settles into SECONDARY,
	// 6-14 SECONDARY, 15 half SECONDARY and half PRIMARY, 16-19 PRIMARY.
	minute := func(n int) time.Time { return start.Add(time.Duration(n) * time.Minute) }
	rows := []BandRow{{Label: "node", Spans: []BandSpan{
		{From: minute(5), To: minute(5).Add(2 * time.Second), State: "RECOVERING"},
		{From: minute(5).Add(2 * time.Second), To: minute(15).Add(30 * time.Second), State: "SECONDARY"},
		{From: minute(15).Add(30 * time.Second), To: minute(20), State: "PRIMARY"},
	}}}
	drawn := Band([]BandGroup{{Rows: rows}}, options)
	line := strings.SplitN(drawn, "\n", 2)[0]
	cells := strings.TrimSpace(line[24:])
	want := ".....SSSSSSSSSSxPPPP"
	if cells != want {
		t.Errorf("band drew\n  %q\nwant\n  %q", cells, want)
	}
}

// TestBandNeverFillsInAnAbsence: a row that says nothing about a stretch has to draw as unknown,
// not as whatever it last was. The whole point of drawing states as a band is that the gaps show.
func TestBandNeverFillsInAnAbsence(t *testing.T) {
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	// Sixteen columns is the narrowest a band is drawn at, so the window is sixteen minutes and
	// each column is one of them.
	options := BandOptions{
		Width: 16 + 24 + 3, Color: false, From: start, To: start.Add(16 * time.Minute),
		Keys:    []BandKey{{State: "PRIMARY", Glyph: 'P', Legend: "PRIMARY"}},
		Unknown: BandKey{Glyph: '.', Legend: "unknown"},
		Mixed:   BandKey{Glyph: 'x', Legend: "mixed"},
	}
	rows := []BandRow{{Label: "node", Spans: []BandSpan{
		{From: start, To: start.Add(6 * time.Minute), State: "PRIMARY"},
	}}}
	line := strings.SplitN(Band([]BandGroup{{Rows: rows}}, options), "\n", 2)[0]
	if cells := strings.TrimSpace(line[24:]); cells != "PPPPPP.........." {
		t.Errorf("band drew %q, want PPPPPP..........", cells)
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
