package label

import (
	"math"
	"sort"
	"strings"
	"time"

	"mongo-log-parser/internal/ftdc"
	"mongo-log-parser/internal/metrics"
)

// Second is one sample of a rule's signal.
//
// Num and Den are kept beside the value because a minute's mean of a ratio is the ratio of its
// sums, not the mean of its seconds: an idle second with one slow request must not weigh as much as
// a busy one. Den is 1 for a signal that is not a ratio.
type Second struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"` // NaN when the second has no ratio: nothing completed in it
	num   float64
	den   float64
}

// Seconds is a rule's signal for one instance, sample by sample, inside the window.
func Seconds(r Rule, samples *ftdc.Samples, instance string, from, to time.Time) []Second {
	if r.Signal.Behind {
		return behind(r, samples, instance, from, to)
	}
	num, ok := summed(r, samples, r.Signal.Num, instance, from, to)
	if !ok {
		return nil
	}
	minus, den := map[int64]float64{}, map[int64]float64{}
	if len(r.Signal.Minus) > 0 {
		if minus, ok = summed(r, samples, r.Signal.Minus, instance, from, to); !ok {
			return nil
		}
	}
	if len(r.Signal.Den) > 0 {
		if den, ok = summed(r, samples, r.Signal.Den, instance, from, to); !ok {
			return nil
		}
	}
	out := make([]Second, 0, len(num))
	for at, n := range num {
		if len(r.Signal.Minus) > 0 {
			m, seen := minus[at]
			if !seen {
				continue
			}
			n -= m
		}
		d := 1.0
		if len(r.Signal.Den) > 0 {
			var seen bool
			if d, seen = den[at]; !seen {
				continue
			}
		}
		out = append(out, Second{At: time.UnixMilli(at).UTC(), num: n, den: d, Value: r.Signal.finish(n, d)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// finish turns a numerator and denominator into the signal's value: divided, scaled, and
// complemented. A ratio with nothing below the line has no value.
func (s Signal) finish(num, den float64) float64 {
	if den <= 0 {
		return math.NaN()
	}
	v := num / den * s.Scale
	if s.Complement {
		v = s.Scale - v
	}
	return v
}

// summed reads a list of paths as one value a second: each gauge as it is, each counter as its rate,
// added together. A second is kept only when every path has a value at it — a sum missing one of its
// terms is a different number, not a smaller one.
func summed(r Rule, samples *ftdc.Samples, entries []string, instance string, from, to time.Time) (map[int64]float64, bool) {
	var out map[int64]float64
	for _, entry := range entries {
		p, ok := resolve(entry, instance, samples)
		if !ok {
			return nil, false
		}
		these := map[int64]float64{}
		for _, point := range metrics.Points(samples, p, r.Signal.Mode, from, to) {
			these[point.At.UnixMilli()] = point.Value
		}
		if out == nil {
			out = these
			continue
		}
		for at, v := range out {
			if add, seen := these[at]; seen {
				out[at] = v + add
			} else {
				delete(out, at)
			}
		}
	}
	return out, out != nil
}

// behind is how far one instance trailed the most advanced instance, sample by sample: the newest
// optime any member reports less this member's. A member reporting nothing, or zero — an arbiter, a
// member down — is left out rather than read as infinitely behind.
func behind(r Rule, samples *ftdc.Samples, instance string, from, to time.Time) []Second {
	pattern := r.Signal.Num[0]
	own, ok := samples.Values[strings.ReplaceAll(pattern, "{i}", instance)]
	if !ok {
		return nil
	}
	everyone := make([][]float64, 0, 8)
	for _, other := range instances(r, samples) {
		everyone = append(everyone, samples.Values[strings.ReplaceAll(pattern, "{i}", other)])
	}
	out := make([]Second, 0, len(own))
	for i, mine := range own {
		at := time.UnixMilli(samples.Timestamps[i]).UTC()
		if math.IsNaN(mine) || mine <= 0 || !inWindow(at, from, to) {
			continue
		}
		newest := mine
		for _, values := range everyone {
			if i < len(values) && !math.IsNaN(values[i]) {
				newest = math.Max(newest, values[i])
			}
		}
		out = append(out, Second{At: at, num: newest - mine, den: 1, Value: r.Signal.finish(newest-mine, 1)})
	}
	return out
}

func inWindow(at, from, to time.Time) bool {
	return (from.IsZero() || !at.Before(from)) && (to.IsZero() || !at.After(to))
}

// instances is every instance a rule runs for: one empty one when it has none, and every disk or
// member the samples carry when it does.
func instances(r Rule, samples *ftdc.Samples) []string {
	pattern := r.Signal.Num[0]
	before, after, instanced := strings.Cut(pattern, "{i}")
	if !instanced {
		return []string{""}
	}
	found := make([]string, 0, 4)
	for p := range samples.Values {
		if !matches(pattern, p) {
			continue
		}
		name := p[len(before) : len(p)-len(after)]
		if r.Signal.RealDevice && !metrics.RealDevice(name) {
			continue
		}
		found = append(found, name)
	}
	sort.Strings(found)
	return found
}

// resolve is the path an entry stands for on one instance, when the samples carry it.
func resolve(entry, instance string, samples *ftdc.Samples) (string, bool) {
	p := strings.ReplaceAll(entry, "{i}", instance)
	_, ok := samples.Values[p]
	return p, ok
}

// missingPath is the first path a rule reads that the samples do not carry, for the instance given
// or the bare pattern when there is none.
func missingPath(r Rule, samples *ftdc.Samples, instance string) string {
	for _, entry := range r.paths() {
		if strings.Contains(entry, "{i}") && instance == "" {
			if len(instances(r, samples)) == 0 {
				return entry
			}
			continue
		}
		if _, ok := resolve(entry, instance, samples); !ok {
			return strings.ReplaceAll(entry, "{i}", instance)
		}
	}
	return ""
}

// resolvedPaths is every concrete path a result was read from, so two labels built from the same
// counter are not offered as each other's explanation.
func resolvedPaths(r Rule, samples *ftdc.Samples, instance string) []string {
	out := make([]string, 0, 4)
	for _, entry := range r.paths() {
		if p, ok := resolve(entry, instance, samples); ok {
			out = append(out, p)
		}
	}
	return out
}
