// Package series is the shape of one metric over a window: the bins, the summary and the range a
// chart is drawn against.
//
// It is data, not a picture. It lives apart from render so that deriving a series from FTDC does
// not drag a drawing package along with it, and so that the JSON a caller reads has one definition.
package series

import (
	"math"
	"time"
)

// Peak is the largest sample of a series and when it happened.
type Peak struct {
	Timestamp string  `json:"timestamp"`
	Value     float64 `json:"value"`
}

// Stats summarizes a series over the requested window.
type Stats struct {
	Min    float64 `json:"min"`
	Avg    float64 `json:"avg"`
	Median float64 `json:"median"`
	P95    float64 `json:"p95"`
	Max    float64 `json:"max"`
	Peak   *Peak   `json:"peak"`
}

// Scale is the value range a chart is drawn against.
type Scale struct {
	Min float64 `json:"scale_min"`
	Max float64 `json:"scale_max"`
}

// Bucket is one time bin of a series.
type Bucket struct {
	Start   string  `json:"start"`
	Samples int     `json:"samples"`
	Min     float64 `json:"min"`
	Avg     float64 `json:"avg"`
	Max     float64 `json:"max"`
}

// The units a value can be in. A series carries its own, because how to print a number is a fact
// about the metric and not about the chart drawing it — and because bytes are counted in powers of
// two everywhere else a DBA looks. MongoDB's own cacheSizeGB, Atlas, Grafana and mongostat all say
// GiB; a tool that answers 68.7G to their 64 GiB is describing the same cache in a dialect nobody
// else speaks.
//
// KiB and MiB are for the metrics that arrive already scaled: /proc/meminfo reports kilobytes that
// are really kibibytes, and serverStatus.mem is in mebibytes. The value is left exactly as the
// server reported it and the scale is applied when it is printed, so nothing downstream has to
// know which of the three it was.
const (
	UnitBytes = "bytes"
	UnitKiB   = "kib"
	UnitMiB   = "mib"
)

// Series is one metric over a window, already binned by the parser.
type Series struct {
	Source string `json:"source"`
	Metric string `json:"metric"`
	// Label is how the metric is shown: a name short enough to read and long enough to be its own
	// among the metrics the bundle carries. It is set by whoever knows that whole set; empty means
	// the renderer should fall back to naming the path on its own.
	Label string `json:"label"`
	Mode  string `json:"mode"`
	// Unit is what the numbers are: one of UnitBytes, UnitKiB, UnitMiB, or empty for a plain count,
	// a rate or a ratio.
	Unit             string `json:"unit,omitempty"`
	SemanticsUnknown bool   `json:"semantics_unknown"`
	// Low says the trough of a bin is its event, not its peak. A pool of tickets or connections is
	// read for how empty it got: binning it to the peak draws a flat line at full for a node that
	// ran out, which is the opposite of what happened.
	Low           bool     `json:"low,omitempty"`
	SampleCount   int      `json:"sample_count"`
	BucketSeconds int64    `json:"bucket_seconds"`
	Stats         *Stats   `json:"stats"`
	Chart         *Scale   `json:"chart"`
	Buckets       []Bucket `json:"buckets"`
}

// Range is the lowest and highest value a chart of this series has to cover.
func (s *Series) Range() (lo, hi float64) {
	if s.Chart != nil {
		return s.Chart.Min, s.Chart.Max
	}
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, bucket := range s.Buckets {
		lo = math.Min(lo, bucket.Min)
		hi = math.Max(hi, bucket.Max)
	}
	return lo, hi
}

func parseStart(value string) float64 {
	stamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return math.NaN()
	}
	return float64(stamp.UnixMilli())
}

// BinnedOver spreads the bins over the requested number of slots, keeping the peak of each; a NaN
// marks a gap. The time span is passed in rather than taken from this series alone, so several
// sources drawn on one chart put the same moment in the same column even when one of them starts
// late or stops early.
func (s *Series) BinnedOver(slots int, first, last float64) []float64 {
	if len(s.Buckets) == 0 {
		return nil
	}
	if !(last > first) {
		first, last = parseStart(s.Buckets[0].Start), parseStart(s.Buckets[len(s.Buckets)-1].Start)
	}
	seconds := float64(s.BucketSeconds)
	if seconds == 0 {
		seconds = 1
	}
	width := int(math.Max(1, math.Min(float64(slots), math.Round((last-first)/(seconds*1000))+1)))
	values := make([]float64, width)
	for i := range values {
		values[i] = math.NaN()
	}
	span := math.Max(seconds*1000, last-first+seconds*1000)
	for _, bucket := range s.Buckets {
		slot := int(math.Min(float64(width-1), math.Floor((parseStart(bucket.Start)-first)/span*float64(width))))
		if slot < 0 {
			slot = 0
		}
		if slot > width-1 {
			slot = width - 1
		}
		// The peak of the bins folded into a slot: a brief spike is the whole point of screening.
		// For a pool it is the other way round, and the trough is the spike.
		if s.Low {
			if math.IsNaN(values[slot]) || bucket.Min < values[slot] {
				values[slot] = bucket.Min
			}
			continue
		}
		if math.IsNaN(values[slot]) || bucket.Max > values[slot] {
			values[slot] = bucket.Max
		}
	}
	return values
}

// Span is the first and last bucket start, in epoch milliseconds. A caller drawing several series
// together needs their extents before it can put the same moment in the same column.
func (s *Series) Span() (first, last float64) {
	if len(s.Buckets) == 0 {
		return 0, 0
	}
	return parseStart(s.Buckets[0].Start), parseStart(s.Buckets[len(s.Buckets)-1].Start)
}

// StartMillis is when a bucket begins, in epoch milliseconds.
func (b Bucket) StartMillis() float64 { return parseStart(b.Start) }

// Kind is how the series should be read: a gauge unless it was derived as a rate.
func (s *Series) Kind() string {
	if s.Mode == "" {
		return "gauge"
	}
	return s.Mode
}
