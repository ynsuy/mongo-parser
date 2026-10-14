// Package metrics turns decoded FTDC samples into the windowed, binned series the report renders.
package metrics

import (
	"math"
	"sort"
	"strings"
	"time"

	"mongo-log-parser/internal/ftdc"
	"mongo-log-parser/internal/series"
)

// Options selects the window, the bin size and how a metric is read.
type Options struct {
	From, To      time.Time
	BucketSeconds int64  // zero picks a readable bin size for the window
	Mode          string // "", "gauge" or "rate"; empty keeps the metric's known mode
	Percent       bool   // draw against 0..100 instead of the observed range
	// TargetBins is how many bins the caller can draw. Zero asks for a chart-sized default.
	TargetBins int
}

type point = Point

// Build produces one renderable series for a metric path.
func Build(samples *ftdc.Samples, source, path string, options Options) series.Series {
	mode, known := options.Mode, true
	if mode == "" {
		mode = Mode(path)
		if known = mode != "unknown"; !known {
			// Nobody declared what this metric is, so read it from its own values. Guessing
			// "gauge" drew every cumulative counter as a staircase that climbs and then flattens,
			// which looks like a number that rose and stayed high when it is only a total.
			mode = inferMode(samples.Values[path])
		}
	}
	built := series.Series{
		Source: source, Metric: path, Mode: mode, SemanticsUnknown: !known,
		BucketSeconds: options.BucketSeconds, Unit: UnitOf(path), Low: Pool(path),
	}
	// A rate of a byte counter is bytes per second, which is still bytes to print.

	return summarize(built, derive(samples, path, mode, options.From, options.To), samples, options)
}

// derive reads a metric as a level or as a per-second rate of a counter.
// Rates are derived before the window is applied so the first in-window rate keeps its predecessor.
func derive(samples *ftdc.Samples, path, mode string, from, to time.Time) []point {
	values, ok := samples.Values[path]
	if !ok {
		return nil
	}
	inWindow := func(at time.Time) bool {
		return (from.IsZero() || !at.Before(from)) && (to.IsZero() || !at.After(to))
	}
	points := make([]point, 0, len(values))
	if mode == "gauge" {
		for i, value := range values {
			at := time.UnixMilli(samples.Timestamps[i]).UTC()
			if math.IsNaN(value) || !inWindow(at) {
				continue
			}
			points = append(points, point{at, value})
		}
		return points
	}
	previous, previousAt, reset := math.NaN(), time.Time{}, false
	for i, value := range values {
		at := time.UnixMilli(samples.Timestamps[i]).UTC()
		if math.IsNaN(value) {
			previous, previousAt, reset = math.NaN(), time.Time{}, false
			continue
		}
		elapsed := at.Sub(previousAt).Milliseconds()
		switch {
		case math.IsNaN(previous) || elapsed <= 0:
			// No baseline yet, so no interval to measure.
		case value < previous:
			// The counter went backwards: the process restarted and this interval has no rate.
			reset = true
		case reset:
			// The first interval after a restart counts from an unknown origin, which would show
			// up as a spike that never happened.
			reset = false
		case inWindow(at):
			points = append(points, point{at, (value - previous) * 1000 / float64(elapsed)})
		}
		previous, previousAt = value, at
	}
	return points
}

// pools are the metrics whose event is running out. What is left of a ticket pool or a connection
// pool is read for how low it went; the peak of a bin is simply "it was full most of the second",
// which is true of a node that ran out of tickets for a minute and of one that never did.
var pools = []string{
	"/concurrentTransactions/read/available",
	"/concurrentTransactions/write/available",
	"/queues/execution/read/available",  // what 8.0 renamed them to
	"/queues/execution/write/available", //
	"serverStatus/connections/available",
}

// Pool reports whether a metric path is one of them.
func Pool(path string) bool {
	for _, suffix := range pools {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// summarize is the half of Build that turns points into a series: the stats, the bins and the
// scale. Build and BuildRatio differ in how they get their points and agree on everything after.
func summarize(built series.Series, points []point, samples *ftdc.Samples, options Options) series.Series {
	built.SampleCount = len(points)
	if len(points) == 0 {
		return built
	}
	if built.BucketSeconds == 0 {
		span := points[len(points)-1].At.Sub(points[0].At)
		built.BucketSeconds = AutoBucketSecondsFor(span, SampleInterval(samples), options.TargetBins)
	}
	values := make([]float64, len(points))
	peak := points[0]
	var sum float64
	for i, p := range points {
		values[i] = p.Value
		sum += p.Value
		if p.Value > peak.Value {
			peak = p
		}
	}
	sort.Float64s(values)
	built.Stats = &series.Stats{
		Min: values[0], Avg: sum / float64(len(values)), Median: median(values),
		P95: values[int(math.Ceil(float64(len(values))*0.95))-1], Max: values[len(values)-1],
		Peak: &series.Peak{Timestamp: peak.At.UTC().Format(time.RFC3339Nano), Value: peak.Value},
	}
	built.Buckets = bucket(points, built.BucketSeconds)
	scale := series.Scale{Min: values[0], Max: values[len(values)-1]}
	if options.Percent {
		scale = series.Scale{Min: 0, Max: 100}
	}
	built.Chart = &scale
	return built
}

func bucket(points []point, seconds int64) []series.Bucket {
	buckets := make([]series.Bucket, 0, len(points))
	index := map[int64]int{}
	for _, p := range points {
		key := p.At.Unix() / seconds * seconds
		at, seen := index[key]
		if !seen {
			index[key] = len(buckets)
			buckets = append(buckets, series.Bucket{
				Start: time.Unix(key, 0).UTC().Format(time.RFC3339),
				Min:   p.Value, Max: p.Value,
			})
			at = len(buckets) - 1
		}
		b := &buckets[at]
		b.Samples++
		b.Avg += p.Value
		b.Min = math.Min(b.Min, p.Value)
		b.Max = math.Max(b.Max, p.Value)
	}
	for i := range buckets {
		buckets[i].Avg /= float64(buckets[i].Samples)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Start < buckets[j].Start })
	return buckets
}

// AutoBucketSecondsFor keeps charts readable with round bin sizes: the finest human interval
// whose bins all fit in `bins` columns, so one bar is exactly one bin. Bins below the sample
// interval only manufacture gaps, so that is the floor.
func AutoBucketSecondsFor(span time.Duration, sampleSeconds int64, bins int) int64 {
	if bins <= 0 {
		bins = 60
	}
	steps := []int64{1, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400}
	for _, step := range steps {
		if step >= sampleSeconds && int64(math.Floor(span.Seconds()/float64(step)))+1 <= int64(bins) {
			return step
		}
	}
	return steps[len(steps)-1]
}

// SampleInterval is the median gap between samples, which is the finest bin the data can support.
func SampleInterval(samples *ftdc.Samples) int64 {
	gaps := make([]float64, 0, len(samples.Timestamps))
	for i := 1; i < len(samples.Timestamps); i++ {
		if gap := samples.Timestamps[i] - samples.Timestamps[i-1]; gap > 0 {
			gaps = append(gaps, float64(gap)/1000)
		}
	}
	if len(gaps) == 0 {
		return 1
	}
	sort.Float64s(gaps)
	return max(1, int64(math.Round(median(gaps))))
}

func median(sorted []float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// inferMode reads whether a metric is a counter from its own values.
//
// A counter only ever climbs, apart from the restarts that set it back. A gauge moves both ways.
// Counting the steps in each direction separates them without a table of every metric MongoDB
// emits, and the ratio — rather than "never falls" — leaves room for the restarts.
func inferMode(values []float64) string {
	rises, falls := 0, 0
	previous := math.NaN()
	for _, value := range values {
		if math.IsNaN(value) {
			previous = math.NaN()
			continue
		}
		if !math.IsNaN(previous) {
			switch {
			case value > previous:
				rises++
			case value < previous:
				falls++
			}
		}
		previous = value
	}
	// A metric that never moved says nothing either way; reading it as a level is the honest guess.
	if rises < 3 || falls*10 > rises {
		return "gauge"
	}
	return "rate"
}

// Point is one derived reading: a level as it was read, or a counter's change per second.
type Point struct {
	At    time.Time
	Value float64
}

// Points is a metric read the way its mode says, every sample in the window, for whoever needs the
// readings rather than a chart of them. A counter's restarts are dropped the way a chart drops them.
func Points(samples *ftdc.Samples, path, mode string, from, to time.Time) []Point {
	return derive(samples, path, mode, from, to)
}
