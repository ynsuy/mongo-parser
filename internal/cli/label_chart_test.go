package cli

import (
	"math"
	"testing"
	"time"

	"github.com/ynsuy/mongo-parser/internal/ftdc"
	"github.com/ynsuy/mongo-parser/internal/label"
)

func TestLabelChartUsesScaledSignalAndRetainsGaps(t *testing.T) {
	at := time.Date(2026, 10, 9, 6, 21, 0, 0, time.UTC)
	samples := &ftdc.Samples{Values: map[string][]float64{"systemMetrics/disks/sda/io_time_ms": {0, 20, 890, math.NaN(), 900, 920}}}
	for i := 0; i < 6; i++ {
		samples.Timestamps = append(samples.Timestamps, at.Add(time.Duration(i)*time.Second).UnixMilli())
	}
	rule := label.Definitions("disk.busy")[0]
	got := labelSignalSeries(rule, "node-a", "sda", samples, time.Time{}, time.Time{}, 1, 100)
	if got.PlotAggregation != "avg" || got.Unit != "%" || got.Source != "node-a" || got.Metric != "disk.busy" || got.SemanticsUnknown || got.Stats == nil {
		t.Fatalf("signal identity lost: %+v", got)
	}
	if got.SampleCount != 3 || got.Stats.Min != 2 || got.Stats.Max != 87 {
		t.Fatalf("expected percentages, not raw ms/s: %+v", got)
	}
	if len(got.Buckets) != 3 || got.Buckets[2].Start != at.Add(5*time.Second).Format(time.RFC3339) {
		t.Fatalf("missing samples were filled: %+v", got.Buckets)
	}
	from := at.Add(2 * time.Second)
	window := labelSignalSeries(rule, "node-a", "sda", samples, from, at.Add(5*time.Second), 1, 100)
	if window.SampleCount != 2 || window.Stats.Max != 87 {
		t.Fatalf("window lost the counter predecessor: %+v", window)
	}
}

func TestLabelChartPreservesRatioAndSignalUnitForTimeViews(t *testing.T) {
	at := time.Date(2026, 10, 9, 6, 21, 0, 0, time.UTC)
	samples := &ftdc.Samples{Timestamps: []int64{at.UnixMilli(), at.Add(time.Second).UnixMilli()}, Values: map[string][]float64{
		"systemMetrics/disks/sda/read_time_ms": {0, 400}, "systemMetrics/disks/sda/write_time_ms": {0, 200},
		"systemMetrics/disks/sda/reads": {0, 10}, "systemMetrics/disks/sda/writes": {0, 5},
	}}
	await := labelSignalSeries(label.Definitions("disk.await")[0], "node-a", "sda", samples, time.Time{}, time.Time{}, 1, 100)
	if await.Unit != "ms" || await.Stats == nil || await.Stats.Max != 40 {
		t.Fatalf("ratio is not milliseconds per request: %+v", await)
	}
	threshold := 95.0
	rule := label.Rule{Name: "example.time_above", View: label.ViewTimeAbove, Threshold: &threshold,
		Signal: label.Signal{Mode: "gauge", Unit: "%", Scale: 1, Num: []string{"value"}}}
	samples.Values = map[string][]float64{"value": {20, 99}}
	timed := labelSignalSeries(rule, "node-a", "", samples, time.Time{}, time.Time{}, 1, 100)
	if rule.Unit() != "s/min" || timed.Unit != "%" || timed.Stats.Max != 99 {
		t.Fatalf("signal was confused with time-view units: %+v", timed)
	}
}
