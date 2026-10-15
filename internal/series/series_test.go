package series

import (
	"math"
	"testing"
)

func bucketsOf(mins, maxes []float64) []Bucket {
	out := make([]Bucket, len(mins))
	for i := range mins {
		out[i] = Bucket{
			Start:   minuteStamp(i),
			Min:     mins[i],
			Max:     maxes[i],
			Avg:     (mins[i] + maxes[i]) / 2,
			Samples: 2,
		}
	}
	return out
}

func minuteStamp(i int) string {
	const base = "2026-10-05T10:0"
	return base + string(rune('0'+i)) + ":00Z"
}

// A pool that emptied inside a bin has to reach the floor of the chart. Drawn from the peak it sits
// at full, which is the same line a pool that never ran out draws.
func TestATroughSeriesPlotsItsLow(t *testing.T) {
	full := Series{BucketSeconds: 60, Buckets: bucketsOf([]float64{128, 0, 128}, []float64{128, 128, 128})}
	first, last := full.Span()
	if got := full.BinnedOver(3, first, last); got[1] != 128 {
		t.Errorf("by peak the middle bin is %v, want 128", got[1])
	}
	pool := full
	pool.Low = true
	got := pool.BinnedOver(3, first, last)
	if got[1] != 0 {
		t.Errorf("by trough the middle bin is %v, want 0 — the moment it ran out", got[1])
	}
	if got[0] != 128 || got[2] != 128 {
		t.Errorf("the bins either side are %v and %v, want 128", got[0], got[2])
	}
	for _, v := range got {
		if math.IsNaN(v) {
			t.Error("a bin with samples came back as a gap")
		}
	}
}
