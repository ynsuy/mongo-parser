package series

import (
	"math"
	"testing"
)

func TestAveragePlotWeightsSamplesAndKeepsExtremaAndGaps(t *testing.T) {
	row := Series{BucketSeconds: 60, PlotAggregation: "avg", Buckets: []Bucket{
		{Start: "2026-10-09T06:00:00Z", Samples: 1, Min: 0, Avg: 10, Max: 100},
		{Start: "2026-10-09T06:01:00Z", Samples: 9, Min: 5, Avg: 20, Max: 80},
		{Start: "2026-10-09T06:03:00Z", Samples: 1, Min: 0, Avg: 30, Max: 90},
	}}
	first, last := row.Span()
	if values := row.BinnedOver(1, first, last); len(values) != 1 || math.Abs(values[0]-220.0/11) > 1e-9 {
		t.Fatalf("collapsed means were not weighted by sample count: %v", values)
	}
	values := row.BinnedOver(4, first, last)
	if !math.IsNaN(values[2]) || values[0] != 10 || values[1] != 20 || values[3] != 30 {
		t.Fatalf("means or gap changed: %v", values)
	}
	if row.Buckets[0].Max != 100 {
		t.Fatal("plotting discarded a sample extremum")
	}
	row.Low = true
	if low := row.BinnedOver(4, first, last); low[0] != 0 {
		t.Fatalf("mean hid depletion: %v", low)
	}
}
