package funnel

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/ynsuy/mongo-parser/internal/ftdc"
	"github.com/ynsuy/mongo-parser/internal/label"
)

// Opened is one label on one node opened up: exactly when it was past its mark, and what it did in
// those seconds.
type Opened struct {
	Result label.Result `json:"result"`
	// Stretches are the runs of seconds past the label's mark, longest first, the first few of
	// StretchCount. A label with no mark — shape only — has none and is read through its episodes.
	Stretches    []label.Stretch `json:"stretches,omitempty"`
	StretchCount int             `json:"stretch_count"`
	// Focus is the windows worth reading closely: the longest stretches, the episodes, or the moment
	// asked for.
	Focus []Focus `json:"focus"`
}

// Focus is one window of an opened label.
type Focus struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Why is what picked the window: "the longest stretch past the mark", "an episode", "asked for".
	Why string `json:"why"`
	// Self is the label itself over the window.
	Self label.Window `json:"self"`
	// Seconds is the window second by second, when the window was asked for. Rows that repeat the
	// row before them are folded into it.
	Seconds []Row `json:"seconds,omitempty"`
	// Evidence is what the logs said in the window, filled by whoever can read them.
	Evidence any `json:"evidence,omitempty"`
}

// Row is one second, or several identical seconds folded together.
type Row struct {
	At     time.Time `json:"at"`
	Repeat int       `json:"repeat,omitempty"` // further seconds that read the same
	Value  float64   `json:"value"`            // the label's signal, in its signal unit
}

// MarshalJSON writes a second with no sample as a null value, not NaN.
func (r Row) MarshalJSON() ([]byte, error) {
	out := struct {
		At     time.Time `json:"at"`
		Repeat int       `json:"repeat,omitempty"`
		Value  *float64  `json:"value"`
	}{At: r.At, Repeat: r.Repeat}
	if !math.IsNaN(r.Value) {
		v := r.Value
		out.Value = &v
	}
	return json.Marshal(out)
}

const (
	stretchGap    = 5 * time.Second  // seconds back under the mark that do not end a stretch
	stretchesKept = 10               // the longest few stretches are listed; the rest are counted
	focusKept     = 3                // windows read closely when none was asked for
	focusPad      = 30 * time.Second // read either side of a stretch, so its edges show
	maxRows       = 900              // fifteen minutes of seconds is the most a table may print
)

// Open opens one label on one node up. samples are the node's decoded FTDC. at, when set, asks for
// the window [at-span/2, at+span/2] second by second instead of the windows the label picks itself.
func Open(target label.Result, samples *ftdc.Samples, from, to, at time.Time, span time.Duration) Opened {
	rule := target.Rule()
	seconds := label.Seconds(rule, samples, target.Instance, from, to)
	opened := Opened{Result: target}
	stretches := label.Stretches(rule, seconds, stretchGap)
	opened.StretchCount = len(stretches)
	opened.Stretches = stretches[:min(len(stretches), stretchesKept)]

	read := func(f Focus, table bool) Focus {
		f.Self = label.Summarize(rule, seconds, f.From, f.To)
		if table {
			f.Seconds = rows(seconds, f.From, f.To)
		}
		return f
	}

	if !at.IsZero() {
		half := span / 2
		opened.Focus = append(opened.Focus, read(Focus{From: at.Add(-half), To: at.Add(half), Why: "asked for"}, true))
		return opened
	}
	for _, s := range opened.Stretches[:min(len(opened.Stretches), focusKept)] {
		why := "a stretch past the mark"
		if len(opened.Focus) == 0 {
			why = "the longest stretch past the mark"
		}
		opened.Focus = append(opened.Focus, read(Focus{From: s.From.Add(-focusPad), To: s.To.Add(focusPad), Why: why}, false))
	}
	// A label with no stretches — shape only, or one that never reached its mark — is read through
	// the episodes and the step its minutes showed.
	if len(opened.Focus) == 0 {
		for _, e := range target.Episodes[:min(len(target.Episodes), focusKept)] {
			opened.Focus = append(opened.Focus, read(Focus{From: e.Start, To: e.End.Add(-time.Second), Why: "an episode"}, false))
		}
		if c := target.Change; c != nil && len(opened.Focus) == 0 {
			opened.Focus = append(opened.Focus, read(Focus{From: c.At.Add(-5 * time.Minute), To: c.At.Add(5 * time.Minute), Why: "the step"}, false))
		}
	}
	sort.SliceStable(opened.Focus, func(i, j int) bool { return opened.Focus[i].From.Before(opened.Focus[j].From) })
	return opened
}

// rows is the window second by second. Identical neighbouring rows fold into one, and the table
// stops at maxRows.
func rows(seconds []label.Second, from, to time.Time) []Row {
	index := make(map[int64]float64, len(seconds))
	for _, s := range seconds {
		if !s.At.Before(from) && !s.At.After(to) {
			index[s.At.Unix()] = s.Value
		}
	}
	keys := make([]int64, 0, len(index))
	for k := range index {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Row, 0, min(len(keys), maxRows))
	for _, k := range keys {
		v, ok := index[k]
		if !ok {
			v = math.NaN()
		}
		v = round(v)
		if n := len(out); n > 0 && same(out[n-1].Value, v) {
			out[n-1].Repeat++
			continue
		}
		if len(out) == maxRows {
			break
		}
		out = append(out, Row{At: time.Unix(k, 0).UTC(), Value: v})
	}
	return out
}

// round keeps three significant figures, so a table of seconds is not a table of float noise.
func round(v float64) float64 {
	if math.IsNaN(v) || v == 0 {
		return v
	}
	digits := 2 - int(math.Floor(math.Log10(math.Abs(v))))
	scale := math.Pow(10, float64(digits))
	return math.Round(v*scale) / scale
}

func same(a, b float64) bool {
	return a == b || (math.IsNaN(a) && math.IsNaN(b))
}
