package render

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mongo-log-parser/internal/series"
)

var update = flag.Bool("update", false, "rewrite the table snapshots")

// The chart layout began as a verified port of dbxray's TypeScript renderer; the numbers inside it
// are ours now, so both are pinned by snapshot. Refresh with -update after a deliberate change.
// The multi-metric report is ours, not a port, so it is pinned by a snapshot. Refresh with -update.
func TestReportSnapshot(t *testing.T) {
	cases := map[string][]series.Series{}
	read(t, "testdata/cases.json", &cases)
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)

	var rendered strings.Builder
	for _, name := range names {
		for _, layout := range []ReportOptions{{Width: 80, ChartHeight: 4}, {Width: 120, ChartHeight: 6}, {Width: 60, ChartHeight: 9}} {
			fmt.Fprintf(&rendered, "== %s width=%d height=%d\n%s\n\n", name, layout.Width, layout.ChartHeight, MetricsReport(cases[name], layout))
		}
		// An overlay is one metric as several sources reported it, so it gets one metric here.
		fmt.Fprintf(&rendered, "== %s overlay width=96\n%s\n\n", name, MetricOverlay(cases[name][:1], OverlayOptions{Width: 96, Height: 9, Color: false}))
	}
	// The comparison views, on one metric three nodes disagree about.
	nodes := asThreeNodes(cases["single"][0])
	fmt.Fprintf(&rendered, "== three nodes stacked width=110\n%s\n\n", MetricsReport(nodes, ReportOptions{Width: 110, ChartHeight: 4}))
	fmt.Fprintf(&rendered, "== three nodes overlay width=110\n%s\n\n", MetricOverlay(nodes, OverlayOptions{Width: 110, Height: 8, Color: false}))
	// A panel is every metric of one stream on one chart, against one value
	// axis, with a legend under it.
	panel := []series.Series{
		withLabel(cases["ranked"][0], "read"),
		withLabel(cases["ranked"][1], "write"),
		withLabel(cases["ranked"][2], "command"),
	}
	for _, width := range []int{80, 120} {
		for _, own := range []bool{false, true} {
			fmt.Fprintf(&rendered, "== panel width=%d own=%v\n%s\n\n", width, own,
				MetricPanel(panel, PanelOptions{Width: width, Height: 6, Title: "Tickets in use", Own: own}))
		}
	}
	fmt.Fprintf(&rendered, "== panel numbers\n%s\n\n", LineTable(panel, TableOptions{Width: 100}))

	// The grid is what --grid draws for several streams: a stream to a column, so nothing is
	// hidden behind anything, and one time axis under the whole board.
	board := append(append([]series.Series(nil), nodes...), asThreeNodes(cases["ranked"][0])...)
	for _, width := range []int{110, 160} {
		fmt.Fprintf(&rendered, "== three nodes grid width=%d\n%s\n\n", width,
			MetricGrid(board, GridOptions{Width: width, ChartHeight: 4, Sources: sourcesOf(board), Selected: 1}))
	}
	const path = "testdata/report.txt"
	if *update {
		if err := os.WriteFile(path, []byte(rendered.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Log("snapshot rewritten")
		return
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run: go test ./internal/render -update)", err)
	}
	if rendered.String() != string(expected) {
		t.Errorf("report layout changed; review and rerun with -update if intended\n--- got ---\n%s", rendered.String())
	}
}

// A grid must fit the terminal at every width and stream count it is offered, and never draw more
// panels than it said would fit.
func TestGridStaysInsideRequestedWidth(t *testing.T) {
	cases := map[string][]series.Series{}
	read(t, "testdata/cases.json", &cases)
	board := asThreeNodes(cases["single"][0])
	for _, width := range []int{48, 60, 80, 110, 160} {
		panels := GridPanels(width)
		if panels < 1 {
			t.Fatalf("width %d claimed room for %d panels", width, panels)
		}
		sources := sourcesOf(board)
		if len(sources) > panels {
			sources = sources[:panels]
		}
		out := MetricGrid(board, GridOptions{Width: width, ChartHeight: 4, Sources: sources, Selected: 0})
		for _, line := range strings.Split(out, "\n") {
			if utf8.RuneCountInString(line) > width {
				t.Errorf("width %d: line of %d runes: %q", width, utf8.RuneCountInString(line), line)
			}
		}
	}
}

// withLabel is a series shown under a name the panel gave it rather than its path's own.
func withLabel(row series.Series, label string) series.Series {
	row.Label = label
	return row
}

// A panel must fit the width it was given, whatever it holds.
func TestPanelFitsTheWidth(t *testing.T) {
	cases := map[string][]series.Series{}
	read(t, "testdata/cases.json", &cases)
	for _, rows := range [][]series.Series{cases["ranked"], cases["single"], cases["magnitudes"]} {
		for _, width := range []int{60, 80, 120, 160} {
			for _, height := range []int{2, 6, 20} {
				out := MetricPanel(rows, PanelOptions{Width: width, Height: height, Title: "Operations"})
				for _, line := range strings.Split(out, "\n") {
					if utf8.RuneCountInString(line) > width {
						t.Errorf("%d series, width %d, height %d: line of %d runes: %q",
							len(rows), width, height, utf8.RuneCountInString(line), line)
					}
				}
			}
		}
	}
}

// PanelLines has to say exactly how tall a panel will be before anything is drawn. A board scrolls
// by panel and lays itself out from that arithmetic; a panel that came out taller than it claimed
// would either overflow the screen or waste a row on every pass.
func TestPanelIsExactlyAsTallAsItClaimed(t *testing.T) {
	cases := map[string][]series.Series{}
	read(t, "testdata/cases.json", &cases)
	for _, plotRows := range []int{MinPlotRows, 5, 9} {
		for _, width := range []int{60, 100, 160} {
			for _, count := range []int{1, 2, 3, 6, 9} {
				rows := cases["magnitudes"][:count]
				names := make([]string, count)
				for i, row := range rows {
					names[i] = nameOf(row)
				}
				lines := strings.Count(MetricPanel(rows, PanelOptions{Width: width, Height: plotRows, Title: "x"}), "\n") + 1
				if want := PanelLines(names, plotRows, width); lines != want {
					t.Errorf("%d series at %d plot rows and width %d drew %d lines, want %d",
						count, plotRows, width, lines, want)
				}
			}
		}
	}
}

// A plot never gets less room than the floor, however little it is offered: below three rows every
// line reads as flat, a spike, or both.
func TestPlotNeverGoesBelowTheFloor(t *testing.T) {
	cases := map[string][]series.Series{}
	read(t, "testdata/cases.json", &cases)
	rows := cases["magnitudes"][:4]
	names := make([]string, len(rows))
	for i, row := range rows {
		names[i] = nameOf(row)
	}
	for _, asked := range []int{-3, 0, 1, MinPlotRows} {
		lines := strings.Count(MetricPanel(rows, PanelOptions{Width: 100, Height: asked, Title: "x"}), "\n") + 1
		if want := PanelLines(names, MinPlotRows, 100); lines != want {
			t.Errorf("asked for %d plot rows, drew %d lines, want %d", asked, lines, want)
		}
	}
}

// Where two lines want the same cell, both have to survive it. The old answer gave the cell to
// whichever series was drawn first, and a line that disappears behind another reads as a line that
// stopped — which is the one thing a chart of six counters must never say.
func TestCrossingLinesAreBothAccountedFor(t *testing.T) {
	rows := []series.Series{
		withLabel(sampleSeries("node-a", "a/up", 0, 0, 2, 4, 6, 8, 10), "up"),
		withLabel(sampleSeries("node-a", "a/down", 0, 10, 8, 6, 4, 2, 0), "down"),
	}
	panel := MetricPanel(rows, PanelOptions{Width: 100, Height: 6, Title: "Crossing"})
	if !strings.ContainsRune(panel, panelMarks(false).cross) {
		t.Errorf("two lines crossed and nothing was marked:\n%s", panel)
	}
	if !strings.Contains(panel, "lines meet") {
		t.Errorf("the legend does not explain the mark:\n%s", panel)
	}
	// Both series still own cells of their own: the mark replaces the meeting point, not a line.
	for _, glyph := range []rune{strokesFor(0).flat, strokesFor(1).flat} {
		if !strings.ContainsRune(panel, glyph) {
			t.Errorf("the line drawn with %q was swallowed whole:\n%s", string(glyph), panel)
		}
	}
	// Two lines that never meet leave the mark off the screen and the note out of the legend.
	apart := []series.Series{
		withLabel(sampleSeries("node-a", "a/high", 0, 9, 10, 9, 10), "high"),
		withLabel(sampleSeries("node-a", "a/low", 0, 0, 0, 0, 0), "low"),
	}
	out := MetricPanel(apart, PanelOptions{Width: 100, Height: 6, Title: "Apart"})
	if strings.ContainsRune(out, panelMarks(false).cross) || strings.ContainsRune(out, panelMarks(false).level) {
		t.Errorf("lines that never meet were marked as meeting:\n%s", out)
	}
	if strings.Contains(out, "lines meet") {
		t.Errorf("the legend explains a mark that is not on the chart:\n%s", out)
	}
}

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

// In colour every line is the same line and colour is what tells them apart; the dash patterns are
// for a chart printed without it, where the glyph is the only channel left.
//
// Two lines at the same value must not hide each other either way. In colour the cell alternates
// between their colours, so a stretch where both sit at seventeen reads as one line in two colours
// — still a line, visibly two of them. Without colour there is nothing to alternate, so it becomes
// a mark instead.
func TestLinesAreToldApartByColourWhenThereIsColour(t *testing.T) {
	together := func(label string, values ...float64) series.Series {
		row := sampleSeries("node-a", "wt/"+label, 0, values...)
		row.Label = label
		return row
	}
	rows := []series.Series{
		together("read", 5, 17, 17, 17, 17, 9, 3),
		together("write", 1, 17, 17, 17, 17, 14, 8),
	}
	colored := MetricPanel(rows, PanelOptions{Width: 90, Height: 5, Title: "Tickets", Color: true})
	plain := MetricPanel(rows, PanelOptions{Width: 90, Height: 5, Title: "Tickets"})

	// One glyph set in colour: nothing from any other pen reaches the chart.
	for _, pen := range strokeSets[1:] {
		if strings.ContainsRune(strings.Join(plotRowsOf(colored), "\n"), pen.flat) {
			t.Errorf("a coloured panel drew a second dash pattern %q:\n%s", string(pen.flat), colored)
		}
	}
	// And no marks, because the colour already says it.
	for _, mark := range []rune{panelMarks(false).level, panelMarks(false).cross} {
		if strings.ContainsRune(strings.Join(plotRowsOf(colored), "\n"), mark) {
			t.Errorf("a coloured panel drew the mark %q instead of alternating:\n%s", string(mark), colored)
		}
	}
	// The row the two lines share carries both of their colours.
	shared := ""
	for _, line := range plotRowsOf(colored) {
		if strings.Count(line, "\x1b[36m") > 0 && strings.Count(line, "\x1b[33m") > 0 {
			shared = line
		}
	}
	if shared == "" {
		t.Errorf("no row alternates between the two lines' colours:\n%s", colored)
	}

	// Printed without colour, the pens come back and so do the marks.
	if !strings.ContainsRune(plain, strokeSets[1].flat) {
		t.Errorf("a panel without colour drew both lines with one dash pattern:\n%s", plain)
	}
	if !strings.ContainsRune(plain, panelMarks(false).level) {
		t.Errorf("a panel without colour lost the overlap mark:\n%s", plain)
	}
	// Either way both readings are the same height, so a board lays out the same with colour or not.
	if strings.Count(colored, "\n") != strings.Count(plain, "\n") {
		t.Error("colour changed how tall the panel is")
	}
}

// A panel has to say how to read it: how long one column is, and what the value axis covers. Those
// are the facts no line can carry itself, and without them a reader cannot tell whether a shape is
// a minute of trouble or an afternoon of it.
func TestPanelNamesItsColumnAndItsScale(t *testing.T) {
	cases := map[string][]series.Series{}
	read(t, "testdata/cases.json", &cases)
	rows := cases["ranked"][:3]
	out := MetricPanel(rows, PanelOptions{Width: 100, Height: 5, Title: "Operations"})
	if !strings.Contains(out, "/col") {
		t.Errorf("the panel does not say what a column is worth:\n%s", out)
	}
	if !strings.Contains(out, "-") {
		t.Errorf("the panel does not say what its axis covers:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if utf8.RuneCountInString(line) > 100 {
			t.Errorf("line of %d runes: %q", utf8.RuneCountInString(line), line)
		}
	}
}

// One value axis is the default and the own-peak reading is the other one. It has to change what is
// drawn and not only what is claimed, the title has to say which is on screen, and the legend has
// to go on printing the real figures — the reading changes the picture, never the facts.
func TestOwnPeakChangesThePanelButNotTheFigures(t *testing.T) {
	rows := []series.Series{
		withLabel(sampleSeries("node-a", "a/big", 0, 0, 500, 1000), "big"),
		withLabel(sampleSeries("node-a", "a/small", 0, 0, 5, 10), "small"),
	}
	shared := MetricPanel(rows, PanelOptions{Width: 80, Height: 5, Title: "Operations"})
	own := MetricPanel(rows, PanelOptions{Width: 80, Height: 5, Title: "Operations", Own: true})
	if shared == own {
		t.Fatal("the own-peak reading drew the same panel as the shared axis")
	}
	if !strings.Contains(shared, "0 - 1.0k") {
		t.Errorf("the shared axis does not name its range:\n%s", shared)
	}
	if !strings.Contains(own, "each to its own peak") {
		t.Errorf("the own-peak reading is not named in the title:\n%s", own)
	}
	// The small series reaches the top of the chart under its own peak and is pressed flat by the
	// shared one; either way the legend prints what it really got to.
	for _, out := range []string{shared, own} {
		if !strings.Contains(out, "small 10.0") {
			t.Errorf("the legend lost the real figures:\n%s", out)
		}
	}
	sharedRows, ownRows := plotRowsOf(shared), plotRowsOf(own)
	if sharedRows[0] == ownRows[0] {
		t.Errorf("the top of the chart is identical under both readings:\n%s\n%s", shared, own)
	}
}

// plotRowsOf is the chart rows of a panel: the ones carrying the value-axis bracket.
func plotRowsOf(panel string) []string {
	out := make([]string, 0, 8)
	for _, line := range strings.Split(panel, "\n") {
		if regexp.MustCompile(`^\s*-?\d+(?:\.\d+)?(?:[A-Za-z/]+)? \|`).MatchString(line) {
			out = append(out, line)
		}
	}
	return out
}

// The axis covers what is drawn and nothing else. The plot takes the peak of every bin, so a lone
// negative sample no bin's peak ever shows must not push the floor below zero, or a cache that is
// never empty is charted against an axis that starts below zero, and part of the height goes to a
// value that could not appear on it.
func TestAxisIgnoresASampleTheChartCannotDraw(t *testing.T) {
	row := sampleSeries("node-a", "wt/cache/bytes", 0, 50, 52, 54, 53)
	// A raw sample far below anything a bin peaks at, the way a counter glitch reaches the scale.
	row.Chart = &series.Scale{Min: -30, Max: 54}
	row.Buckets[1].Min = -30
	row.Label = "in cache"
	out := MetricPanel([]series.Series{row}, PanelOptions{Width: 80, Height: 5, Title: "Cache"})
	if !strings.Contains(out, "0 - 54.0") {
		t.Errorf("the axis does not read from zero:\n%s", out)
	}
	for _, line := range plotRowsOf(out) {
		if strings.Contains(line, "-") && strings.Contains(line, "|") && strings.HasPrefix(strings.TrimSpace(line), "-") {
			t.Errorf("the axis carries a negative label for data that is never negative:\n%s", out)
		}
	}
	// Data that really is drawn below zero still gets the room it needs.
	below := sampleSeries("node-a", "sys/drift", 0, -8, 4, -6)
	below.Label = "drift"
	if out := MetricPanel([]series.Series{below}, PanelOptions{Width: 80, Height: 5, Title: "Drift"}); !strings.Contains(out, "-8") {
		t.Errorf("a series that goes negative lost its floor:\n%s", out)
	}
}

// A panel with nothing to draw says so rather than drawing an empty chart, which would claim the
// server reported nothing when the truth is that it does not report that at all.
func TestEmptyPanelSaysSo(t *testing.T) {
	out := MetricPanel(nil, PanelOptions{Width: 80, Height: 5, Title: "Replication"})
	if !strings.Contains(out, "Replication") || !strings.Contains(out, "nothing in this bundle") {
		t.Errorf("empty panel: %q", out)
	}
}

// Output must never print wider than asked; the terminal has no horizontal scroll.
func TestReportStaysInsideRequestedWidth(t *testing.T) {
	cases := map[string][]series.Series{}
	read(t, "testdata/cases.json", &cases)
	for name, rows := range cases {
		for _, width := range []int{50, 64, 80, 100, 160, 200} {
			for _, height := range []int{3, 5, 12} {
				for _, line := range strings.Split(MetricsReport(rows, ReportOptions{Width: width, ChartHeight: height}), "\n") {
					if utf8.RuneCountInString(line) > width {
						t.Errorf("%s at width %d height %d: line of %d runes: %q",
							name, width, height, utf8.RuneCountInString(line), line)
					}
				}
			}
		}
	}
}

// A metric is shown by the two segments that identify it, and never cut: a name is how a reader
// gets back to the data, and half a name gets nobody anywhere.
func TestMetricNamesAreShownWholeAndShort(t *testing.T) {
	cases := map[string][]series.Series{}
	read(t, "testdata/cases.json", &cases)
	for name, rows := range cases {
		for _, width := range []int{100, 140, 200} {
			report := MetricsReport(rows, ReportOptions{Width: width, ChartHeight: 4})
			for _, row := range rows {
				short := MetricName(row.Metric)
				if !strings.Contains(report, short) {
					t.Errorf("%s at width %d: %q is not printed", name, width, short)
				}
			}
		}
	}
}

// The name column has a floor, so a board of short names is not narrower than the next one, and no
// ceiling, so a long name is printed rather than cut.
func TestNameColumnHasAFloorAndNoCeiling(t *testing.T) {
	short := []series.Series{sampleSeries("node-a", "a/b", 0, 1, 2)}
	long := []series.Series{sampleSeries("node-a", "wiredTiger/checkpoint-cleanup/pages read into cache due to obsolete time window", 0, 1, 2)}
	widthOf := func(rows []series.Series) int {
		for _, line := range strings.Split(MetricsReport(rows, ReportOptions{Width: 200, ChartHeight: 3}), "\n") {
			if strings.HasPrefix(line, "|") {
				return utf8.RuneCountInString(strings.SplitN(line[1:], "|", 2)[0])
			}
		}
		return 0
	}
	if got := widthOf(short); got < metricColumn {
		t.Errorf("a short name drew a %d-wide column, under the %d floor", got, metricColumn)
	}
	whole := MetricName(long[0].Metric)
	if !strings.Contains(MetricsReport(long, ReportOptions{Width: 200, ChartHeight: 3}), whole) {
		t.Errorf("the long name %q was cut", whole)
	}
}

// Colour is decoration: it must not change a single character of the layout.
func TestColorLeavesLayoutAlone(t *testing.T) {
	cases := map[string][]series.Series{}
	read(t, "testdata/cases.json", &cases)
	rows := cases["ranked"]
	plain := MetricsReport(rows, ReportOptions{Width: 120, ChartHeight: 4})
	colored := MetricsReport(rows, ReportOptions{Width: 120, ChartHeight: 4, Color: true})
	if plain == colored {
		t.Fatal("colour was requested but nothing changed")
	}
	stripped := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(colored, "")
	if stripped != plain {
		t.Errorf("colour changed the layout\n--- stripped ---\n%s\n--- plain ---\n%s", stripped, plain)
	}
}

func TestEmptyInputs(t *testing.T) {
	if got := MetricsReport(nil, ReportOptions{Width: 100, ChartHeight: 4}); got != "No metrics to show." {
		t.Errorf("empty report: %q", got)
	}
	empty := series.Series{Metric: "serverStatus/x", Mode: "gauge", Source: "node-a"}
	want := "serverStatus.x | gauge | 1 source\n-- node-a\nNo samples in this window."
	if got := MetricOverlay([]series.Series{empty}, OverlayOptions{Width: 80, Height: 9, Color: false}); got != want {
		t.Errorf("empty chart: %q", got)
	}
	if got := MetricOverlay(nil, OverlayOptions{Width: 80, Height: 9, Color: false}); got != "No metrics to show." {
		t.Errorf("empty overlay: %q", got)
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

func read(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatal(err)
	}
}

// asThreeNodes is one metric as three nodes reported it: the same shape at a third and a ninth of
// the magnitude, which is the case a shared value axis exists to show honestly.
func asThreeNodes(base series.Series) []series.Series {
	rows := make([]series.Series, 0, 3)
	for i, name := range []string{"node-a", "node-b", "node-c"} {
		factor := 1 / float64(1+2*i)
		row := base
		row.Source = name
		row.Chart = nil
		row.Buckets = make([]series.Bucket, len(base.Buckets))
		for j, bucket := range base.Buckets {
			bucket.Min, bucket.Avg, bucket.Max = bucket.Min*factor, bucket.Avg*factor, bucket.Max*factor
			row.Buckets[j] = bucket
		}
		if base.Stats != nil {
			stats := *base.Stats
			stats.Min, stats.Avg, stats.Median = stats.Min*factor, stats.Avg*factor, stats.Median*factor
			stats.P95, stats.Max = stats.P95*factor, stats.Max*factor
			row.Stats = &stats
		}
		rows = append(rows, row)
	}
	return rows
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

// A byte panel says so on its axis, its range and its legend — all three, or a reader cross-checking
// one figure against a dashboard finds two of them in a different dialect.
func TestBytePanelIsBinaryThroughout(t *testing.T) {
	cache := sampleSeries("node-a", "serverStatus/wiredTiger/cache/bytes currently in the cache", 0,
		44023414784, 45097156608, 44560285696)
	cache.Label, cache.Unit = "in cache", series.UnitBytes
	configured := sampleSeries("node-a", "serverStatus/wiredTiger/cache/maximum bytes configured", 0,
		68719476736, 68719476736, 68719476736)
	configured.Label, configured.Unit = "configured", series.UnitBytes

	out := MetricPanel([]series.Series{cache, configured}, PanelOptions{Width: 100, Height: 5, Title: "Cache"})
	for _, want := range []string{"0 - 64.0Gi", "in cache 42.0Gi", "configured 64.0Gi"} {
		if !strings.Contains(out, want) {
			t.Errorf("the panel does not carry %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "68.7G") {
		t.Errorf("the panel still prints a decimal byte figure:\n%s", out)
	}

	// A panel that mixes units cannot label its axis in any of them, and must not pick one.
	queue := sampleSeries("node-a", "serverStatus/globalLock/currentQueue/total", 0, 4, 120, 8)
	queue.Label = "queued"
	mixed := MetricPanel([]series.Series{cache, queue}, PanelOptions{Width: 100, Height: 5, Title: "Mixed"})
	if strings.Contains(mixed, "- 45.1Gi") || strings.Contains(mixed, "- 42.0Gi") {
		t.Errorf("a mixed-unit panel labelled its axis in one line's unit:\n%s", mixed)
	}
	// The lines still carry their own units in the legend, where each figure belongs to one series.
	if !strings.Contains(mixed, "in cache 42.0Gi") || !strings.Contains(mixed, "queued 120") {
		t.Errorf("a mixed-unit panel lost a line's own unit:\n%s", mixed)
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

	// Without the flag the first column still gives, which is what a table of metric paths wants.
	columns[1].Elastic = false
	if out := Table(columns, TableOptions{Width: 60}); strings.Contains(out, "critical") {
		t.Errorf("the default squeeze moved off the first column:\n%s", out)
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

// ASCII drawing must retain missing buckets and distinguish the series without relying on color.
func TestPlainChartsAndTablesUseASCIIAndPreserveInputText(t *testing.T) {
	rows := []series.Series{sampleSeries("node-a", "serverStatus/x", 0, 1, 9, 3), sampleSeries("node-b", "serverStatus/x", 0, 3, 2, 7)}
	for name, output := range map[string]string{
		"report":  MetricsReport(rows, ReportOptions{Width: 100, ChartHeight: 5}),
		"overlay": MetricOverlay(rows, OverlayOptions{Width: 100, Height: 5}),
		"panel":   MetricPanel(rows, PanelOptions{Width: 100, Height: 5, Title: "Metrics"}),
		"grid":    MetricGrid(rows, GridOptions{Width: 100, ChartHeight: 5, Selected: -1}),
		"table":   Table([]Column{{Title: "Node", Values: []string{"node-a"}}}, TableOptions{Width: 80}),
	} {
		for _, r := range output {
			if r > 127 || r == 27 {
				t.Fatalf("%s has non-ASCII drawing or escape: %q", name, output)
			}
		}
	}
	pens := map[rune]bool{}
	for _, pen := range strokeSets {
		if pen.flat == gapMark || pens[pen.flat] {
			t.Fatalf("ambiguous ASCII series marker: %q", pen.flat)
		}
		pens[pen.flat] = true
	}
	text := "müşteri-节点"
	output := Table([]Column{{Title: "Node", Values: []string{text}}}, TableOptions{Width: 80})
	if !strings.Contains(output, text) {
		t.Fatalf("input text changed: %q", output)
	}
}
