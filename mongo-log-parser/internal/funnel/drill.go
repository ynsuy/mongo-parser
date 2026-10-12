package funnel

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"mongo-log-parser/internal/ftdc"
	"mongo-log-parser/internal/label"
)

// Opened is one label on one node opened up: when exactly it was past its mark, and what everything
// that pairs with it did in those same seconds.
type Opened struct {
	Result label.Result `json:"result"`
	// Stretches are the runs of seconds past the label's mark, longest first, the first few of
	// StretchCount. A label with no mark — shape only — has none and is read through its episodes.
	Stretches    []label.Stretch `json:"stretches,omitempty"`
	StretchCount int             `json:"stretch_count"`
	// Focus is the windows worth reading closely: the longest stretches, the episodes, or the moment
	// asked for, each with the paired labels read over it.
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
	// Pairs is every label the rule pairs with, on the same node, over the same window, beside its
	// usual level across the whole window.
	Pairs []Pair `json:"pairs,omitempty"`
	// Seconds is the window second by second, the label and its pairs side by side, when the window
	// was asked for. Rows that repeat the row before them are folded into it.
	Seconds []Row `json:"seconds,omitempty"`
	// Columns names each row's values in order: the label, then each pair the node has.
	Columns []string `json:"columns,omitempty"`
	// Evidence is what the logs said in the window, filled by whoever can read them.
	Evidence any `json:"evidence,omitempty"`
}

// Pair is one paired label read over a focus window.
type Pair struct {
	Label      string       `json:"label"`
	Instance   string       `json:"instance,omitempty"`
	Unit       string       `json:"unit"`
	SignalUnit string       `json:"signal_unit"`
	View       string       `json:"view"`
	Window     label.Window `json:"window"`
	// Usual is the pair over the whole window it was labelled on, read the same way as Window, so
	// the two can be set side by side: what it did in these seconds against what it usually did.
	Usual label.Window `json:"usual"`
	// Missing says why the pair could not be read on this node, when it could not.
	Missing string `json:"missing,omitempty"`
	rule    label.Rule
}

// Rule is the definition the pair was read with; empty for a pair the node did not have.
func (p Pair) Rule() label.Rule { return p.rule }

// Row is one second, or several identical seconds folded together.
type Row struct {
	At     time.Time `json:"at"`
	Repeat int       `json:"repeat,omitempty"` // further seconds that read the same
	Values Values    `json:"values"`           // the label, then each pair the node has, in their signal units
}

// Values are one second's readings; a second a column has no sample for is NaN, written as null.
type Values []float64

func (v Values) MarshalJSON() ([]byte, error) {
	out := make([]*float64, len(v))
	for i := range v {
		if !math.IsNaN(v[i]) {
			out[i] = &v[i]
		}
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

// Open opens one label on one node up. results are every label of that node, so the pairs can be
// read and their usual levels known; samples are the node's decoded FTDC. at, when set, asks for the
// window [at-span/2, at+span/2] second by second instead of the windows the label picks itself.
func Open(target label.Result, results []label.Result, samples *ftdc.Samples, from, to, at time.Time, span time.Duration) Opened {
	rule := target.Rule()
	seconds := label.Seconds(rule, samples, target.Instance, from, to)
	opened := Opened{Result: target}
	stretches := label.Stretches(rule, seconds, stretchGap)
	opened.StretchCount = len(stretches)
	opened.Stretches = stretches[:min(len(stretches), stretchesKept)]

	pairs := pairsOf(target, results, samples, from, to)
	read := func(f Focus, table bool) Focus {
		f.Self = label.Summarize(rule, seconds, f.From, f.To)
		for _, p := range pairs {
			if p.Missing == "" {
				p.Window = label.Summarize(p.rule, p.seconds(samples, from, to), f.From, f.To)
			}
			f.Pairs = append(f.Pairs, p.Pair)
		}
		if table {
			f.Seconds = rows(rule, seconds, pairs, samples, f.From, f.To, from, to)
			f.Columns = columns(target, f.Pairs)
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

// pairing is a pair with what it takes to read its seconds.
type pairing struct {
	Pair
	instance string
}

func (p pairing) seconds(samples *ftdc.Samples, from, to time.Time) []label.Second {
	return label.Seconds(p.rule, samples, p.instance, from, to)
}

// pairsOf is every label the target's rule pairs with, on the same node. A paired label with
// instances of its own — a disk beside a ticket pool — is read on each of them; one with the same
// instance as the target, when the target has one, on that alone.
func pairsOf(target label.Result, results []label.Result, samples *ftdc.Samples, from, to time.Time) []pairing {
	out := make([]pairing, 0, 8)
	for _, name := range target.Rule().PairsWith {
		found := false
		for _, r := range results {
			if r.Source != target.Source || r.Label != name {
				continue
			}
			if target.Instance != "" && r.Instance != "" && r.Instance != target.Instance && sameKind(target, r) {
				continue
			}
			found = true
			p := pairing{Pair: Pair{Label: r.Label, Instance: r.Instance, Unit: r.Unit, SignalUnit: r.SignalUnit,
				View: r.View, rule: r.Rule()}, instance: r.Instance}
			p.Usual = label.Summarize(p.rule, p.seconds(samples, from, to), from, to)
			out = append(out, p)
		}
		if !found {
			out = append(out, pairing{Pair: Pair{Label: name, Missing: "not labelled on this node"}})
		}
	}
	return out
}

// sameKind reports whether two labels' instances are the same kind of thing — two disk labels, two
// member labels — so that only the same instance is paired.
func sameKind(a, b label.Result) bool {
	kind := func(r label.Result) string {
		p := r.Rule().Signal.Num[0]
		return p[:strings.Index(p, "{i}")]
	}
	return kind(a) == kind(b)
}

// rows is the window second by second: the label's signal, then each pair's. Identical neighbouring
// rows fold into one, and the table stops at maxRows.
func rows(rule label.Rule, seconds []label.Second, pairs []pairing, samples *ftdc.Samples, from, to, windowFrom, windowTo time.Time) []Row {
	columns := make([]map[int64]float64, 0, len(pairs)+1)
	index := func(ss []label.Second) map[int64]float64 {
		out := make(map[int64]float64, len(ss))
		for _, s := range ss {
			if !s.At.Before(from) && !s.At.After(to) {
				out[s.At.Unix()] = s.Value
			}
		}
		return out
	}
	columns = append(columns, index(seconds))
	for _, p := range pairs {
		if p.Missing == "" { // a pair the node does not have has no seconds to print
			columns = append(columns, index(p.seconds(samples, windowFrom, windowTo)))
		}
	}
	keys := make([]int64, 0, len(columns[0]))
	for k := range columns[0] {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Row, 0, min(len(keys), maxRows))
	for _, k := range keys {
		values := make(Values, len(columns))
		for i, c := range columns {
			v, ok := c[k]
			if !ok {
				v = math.NaN()
			}
			values[i] = round(v)
		}
		if n := len(out); n > 0 && same(out[n-1].Values, values) {
			out[n-1].Repeat++
			continue
		}
		if len(out) == maxRows {
			break
		}
		out = append(out, Row{At: time.Unix(k, 0).UTC(), Values: values})
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

func same(a, b []float64) bool {
	for i := range a {
		if a[i] != b[i] && !(math.IsNaN(a[i]) && math.IsNaN(b[i])) {
			return false
		}
	}
	return true
}

// columns names the columns of a focus window's seconds: the label, then each pair the node has.
func columns(target label.Result, pairs []Pair) []string {
	out := []string{fmt.Sprintf("%s (%s)", target.Label, target.SignalUnit)}
	for _, p := range pairs {
		if p.Missing != "" {
			continue
		}
		name := strings.TrimSpace(p.Label + " " + p.Instance)
		if p.SignalUnit != "" {
			name += " (" + p.SignalUnit + ")"
		}
		out = append(out, name)
	}
	return out
}
