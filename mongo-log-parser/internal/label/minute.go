package label

import (
	"math"
	"sort"
	"time"
)

// Minute is what one minute's seconds came to. Every rule keeps all of these; its view picks the one
// its bands and shape are read on, and the others are there for whoever opens the label up.
type Minute struct {
	At time.Time `json:"at"`
	// Mean is the minute's level: the ratio of the minute's sums for a ratio, the mean otherwise.
	Mean float64 `json:"mean"`
	Max  float64 `json:"max"`
	Min  float64 `json:"min"`
	// Past is how many seconds of the minute the signal spent past the rule's threshold, scaled to a
	// full minute when samples were missing from it. Zero for a rule without a threshold.
	Past float64 `json:"past,omitempty"`
	// Samples is how many seconds the minute held.
	Samples int `json:"samples"`
}

// Value is the minute read through the rule's view.
func (m Minute) Value(r Rule) float64 {
	switch r.View {
	case ViewMax:
		return m.Max
	case ViewMin:
		return m.Min
	case ViewTimeAbove, ViewTimeBelow:
		return m.Past
	}
	return m.Mean
}

// Minutes folds a rule's seconds into minutes.
//
// A minute is kept only when it holds at least half the seconds a full minute holds: the reading at
// the edge of the window, or either side of a gap, is a few seconds and not a minute. A ratio's
// minute is also dropped when its divisor summed to less than the rule's min_den.
func Minutes(r Rule, seconds []Second) []Minute {
	type acc struct {
		num, den        float64
		max, min        float64
		past, n, valued int
	}
	byMinute := map[int64]*acc{}
	full := 0
	for _, s := range seconds {
		key := s.At.Unix() / 60
		a := byMinute[key]
		if a == nil {
			a = &acc{max: math.Inf(-1), min: math.Inf(1)}
			byMinute[key] = a
		}
		a.n++
		full = max(full, a.n)
		a.num += s.num
		a.den += s.den
		if math.IsNaN(s.Value) {
			continue
		}
		a.valued++
		a.max, a.min = math.Max(a.max, s.Value), math.Min(a.min, s.Value)
		if r.Past(s.Value) {
			a.past++
		}
	}
	out := make([]Minute, 0, len(byMinute))
	for key, a := range byMinute {
		if a.n*2 < full || a.den <= 0 || a.den < r.Signal.MinDen || a.valued == 0 {
			continue
		}
		m := Minute{At: time.Unix(key*60, 0).UTC(), Mean: r.Signal.finish(a.num, a.den), Max: a.max, Min: a.min, Samples: a.n}
		if r.Timed() {
			m.Past = math.Round(float64(a.past)/float64(a.valued)*60*10) / 10
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}
