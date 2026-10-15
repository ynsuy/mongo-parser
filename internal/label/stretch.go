package label

import (
	"math"
	"sort"
	"time"
)

// A label is opened up second by second: exactly when it went past its mark and came back, how long
// each stretch lasted, and what the labels it pairs with did inside those same seconds. The minutes
// say where to look; the seconds say what happened there.

// Marked reports whether one second counts towards a stretch: past the threshold for a time view,
// at or past the elevated mark for any other label that has one. A shape-only label has no mark,
// and is opened up through its episodes instead.
func (r Rule) Marked(v float64) bool {
	if math.IsNaN(v) {
		return false
	}
	if r.Timed() {
		return r.Past(v)
	}
	if r.Elevated == nil {
		return false
	}
	if r.Direction == "down" {
		return v <= *r.Elevated
	}
	return v >= *r.Elevated
}

// Stretch is a run of seconds a label spent past its mark.
type Stretch struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"` // the last marked second
	// Seconds is how many seconds inside the stretch were marked; a stretch bridges gaps of up to the
	// joining gap, so it can be shorter than To less From.
	Seconds int       `json:"seconds"`
	Worst   float64   `json:"worst"` // the signal at its most extreme second inside the stretch
	WorstAt time.Time `json:"worst_at"`
}

// Length is the stretch from its first marked second to the end of its last.
func (s Stretch) Length() time.Duration { return s.To.Sub(s.From) + time.Second }

// Stretches is every run of marked seconds, joining runs separated by no more than gap, longest
// first. A pool that empties for three seconds, recovers for one and empties again is one stretch,
// not two.
func Stretches(r Rule, seconds []Second, gap time.Duration) []Stretch {
	lowest := r.Direction == "down" || r.View == ViewTimeBelow
	out := make([]Stretch, 0, 8)
	var open *Stretch
	for _, s := range seconds {
		if !r.Marked(s.Value) {
			continue
		}
		if open != nil && s.At.Sub(open.To) <= gap+time.Second {
			open.To = s.At
			open.Seconds++
			if (lowest && s.Value < open.Worst) || (!lowest && s.Value > open.Worst) {
				open.Worst, open.WorstAt = s.Value, s.At
			}
			continue
		}
		if open != nil {
			out = append(out, *open)
		}
		open = &Stretch{From: s.At, To: s.At, Seconds: 1, Worst: s.Value, WorstAt: s.At}
	}
	if open != nil {
		out = append(out, *open)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seconds > out[j].Seconds })
	return out
}

// Window is what a label did inside one stretch of time, in its own terms: the level over the
// window, its extreme seconds, and for a time view how many of those seconds were past the
// threshold. It is how a paired label is read beside the one being opened up.
type Window struct {
	Mean    float64 `json:"mean"`
	Max     float64 `json:"max"`
	Min     float64 `json:"min"`
	Past    int     `json:"past,omitempty"` // seconds past the threshold, for a time view
	Samples int     `json:"samples"`
}

// Summarize reads a label's seconds between from and to, inclusive; a zero bound is open. Samples
// is zero when the label had no seconds there.
func Summarize(r Rule, seconds []Second, from, to time.Time) Window {
	var num, den float64
	w := Window{Max: math.Inf(-1), Min: math.Inf(1)}
	for _, s := range seconds {
		if !inWindow(s.At, from, to) {
			continue
		}
		num, den = num+s.num, den+s.den
		if math.IsNaN(s.Value) {
			continue
		}
		w.Samples++
		w.Max, w.Min = math.Max(w.Max, s.Value), math.Min(w.Min, s.Value)
		if r.Past(s.Value) {
			w.Past++
		}
	}
	if w.Samples == 0 {
		return Window{}
	}
	w.Mean = r.Signal.finish(num, den)
	return w
}
