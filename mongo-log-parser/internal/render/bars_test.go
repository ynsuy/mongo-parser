package render

import (
	"strings"
	"testing"

	"mongo-log-parser/internal/series"
)

func TestBarChartDrawsLevelsZerosAndGaps(t *testing.T) {
	row := series.Series{Source: "node-a", Metric: "ops", Label: "ops", BucketSeconds: 60, Buckets: []series.Bucket{
		{Start: "2026-10-09T06:00:00Z", Samples: 1, Min: 8, Avg: 8, Max: 8},
		{Start: "2026-10-09T06:01:00Z", Samples: 1, Min: 0, Avg: 0, Max: 0},
		{Start: "2026-10-09T06:03:00Z", Samples: 1, Min: 4, Avg: 4, Max: 4},
		{Start: "2026-10-09T06:04:00Z", Samples: 1, Min: 1, Avg: 1, Max: 1},
	}, Stats: &series.Stats{Max: 8, Peak: &series.Peak{Timestamp: "2026-10-09T06:00:00Z", Value: 8}}}
	out := BarChart([]series.Series{row}, BarOptions{Width: 80, Height: 2})
	lines := strings.Split(out, "\n")
	t.Log("\n" + out)
	// top row: 8 full, 0 blank, gap blank, 4 blank, 1 blank
	if got := []rune(lines[1])[barGutter+2:]; string(got) != "█" {
		t.Fatalf("top row %q", string(got))
	}
	if got := []rune(lines[2])[barGutter+2:]; string(got) != "█ ·█▂" {
		t.Fatalf("bottom row %q", string(got))
	}
	if !strings.Contains(lines[0], "max 8.00 @06:00:00") || !strings.Contains(lines[len(lines)-1], "bar = 1m peak · · = no data") {
		t.Fatalf("header or note lost:\n%s", out)
	}
}

func TestBarScaleFloatsALevelFarFromZero(t *testing.T) {
	level := series.Series{Buckets: []series.Bucket{
		{Start: "2026-10-09T06:00:00Z", Samples: 1, Min: 12200, Avg: 12200, Max: 12200},
		{Start: "2026-10-09T06:01:00Z", Samples: 1, Min: 13200, Avg: 13200, Max: 13200},
	}}
	if base, top := barScale([]series.Series{level}); base != 12000 || top != 13200 {
		t.Fatalf("level scale %v..%v, want 12000..13200", base, top)
	}
	level.Buckets[0].Min, level.Buckets[0].Avg, level.Buckets[0].Max = 4000, 4000, 4000
	if base, _ := barScale([]series.Series{level}); base != 0 {
		t.Fatalf("wide range floated to %v, want 0", base)
	}
}
