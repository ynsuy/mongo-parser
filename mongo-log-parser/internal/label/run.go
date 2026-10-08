package label

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"mongo-log-parser/internal/ftdc"
)

// Result is what one label read on one node, for one instance when it has them.
//
// Every number is in the label's unit — the signal's, or seconds a minute for a time view — except
// PeakSecond, which is the signal itself at its most extreme second.
type Result struct {
	Source   string `json:"source"`
	Label    string `json:"label"`
	Title    string `json:"title"`
	Instance string `json:"instance,omitempty"`
	Version  string `json:"version,omitempty"` // the server version the stream reported
	Unit     string `json:"unit"`
	View     string `json:"view"`
	// Minutes is how many minutes of the window had a reading.
	Minutes int     `json:"minutes"`
	Typical float64 `json:"typical"` // the median minute
	P95     float64 `json:"p95"`
	// Peak is the worst minute: the highest, or the lowest for a label read downwards.
	Peak   float64   `json:"peak"`
	PeakAt time.Time `json:"peak_at"`
	// PeakSecond is the signal's most extreme single second, in the signal's unit: the highest, or
	// the lowest for a label whose trouble is a low value. A minute mean hides it; this does not.
	PeakSecond   float64   `json:"peak_second"`
	PeakSecondAt time.Time `json:"peak_second_at"`
	SignalUnit   string    `json:"signal_unit"`
	// PastSeconds is, for a time view, every second of the window spent past the threshold.
	PastSeconds float64 `json:"past_seconds,omitempty"`
	// Level and PeakLevel are the fuzzy degrees at the typical and at the peak minute, only the
	// bands the value belongs to at all.
	Level     Degrees `json:"level,omitempty"`
	PeakLevel Degrees `json:"peak_level,omitempty"`
	// Shape is one of constant, steady, fluctuating, spiky, episodic, periodic, step_up, step_down,
	// rising, falling.
	Shape    string    `json:"shape"`
	Change   *Change   `json:"change,omitempty"`
	Episodes []Episode `json:"episodes,omitempty"`
	// Spikes counts excursions too short to be an episode: under three minutes.
	Spikes int `json:"spikes,omitempty"`
	// Periodic is set when the excursions came back at a regular interval: a job, not an accident.
	Periodic *Periodic `json:"periodic,omitempty"`
	Duty     *Duty     `json:"duty,omitempty"`
	// Related is the other labels on the same node that moved with this one, strongest first.
	Related []Related `json:"related,omitempty"`

	rule    Rule
	minutes []Minute
	paths   []string
}

// Rule is the definition the result was read with: for a label defined per version, the one that
// matched the node.
func (r Result) Rule() Rule { return r.rule }

// MinuteSeries is the minutes the result was read from.
func (r Result) MinuteSeries() []Minute { return r.minutes }

// Change is a step: the level the label sat at before a moment and the one it sat at after.
type Change struct {
	At     time.Time `json:"at"`
	Before float64   `json:"before"`
	After  float64   `json:"after"`
}

// Episode is a stretch where the label rose clearly above its own usual level and came back, or had
// not come back when the window ended.
type Episode struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Minutes  int       `json:"minutes"`
	Mean     float64   `json:"mean"`
	Peak     float64   `json:"peak"`
	Baseline float64   `json:"baseline"`
	Open     bool      `json:"open,omitempty"` // still elevated when the window ended
}

// Periodic is excursions that recurred at a steady interval: an hourly job, a nightly backup, a cron
// every ten minutes. It is the shape a recurring cause leaves, and the one most easily mistaken for
// noise when each occurrence is looked at on its own.
type Periodic struct {
	Every       time.Duration `json:"every"`
	Occurrences int           `json:"occurrences"`
	First       time.Time     `json:"first"`
	Last        time.Time     `json:"last"`
}

// Duty is how much of the window was spent at or past the rule's elevated mark.
type Duty struct {
	Mark  float64 `json:"mark"`
	Share float64 `json:"share"`
	Label string  `json:"label"` // never, rarely, intermittently, mostly, always
}

// Skip is a label that produced nothing for a node, and why. A label that is simply absent reads as
// "nothing happened"; the reason says whether the server never had the metric, the bundle lost it,
// or there was too little of it to describe.
type Skip struct {
	Source string `json:"source"`
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

// minMinutes is the least a series needs before its shape means anything.
const minMinutes = 5

// Run reads every label off one node's samples. version is the server version the stream reported,
// empty when unknown.
func Run(samples *ftdc.Samples, source, version string, from, to time.Time) ([]Result, []Skip) {
	if samples == nil {
		return nil, nil
	}
	out, skipped := make([]Result, 0, len(Rules)), make([]Skip, 0, 8)
	skip := func(name, reason string) {
		skipped = append(skipped, Skip{Source: source, Label: name, Reason: reason})
	}
	for _, name := range Names() {
		r, why := Choose(name, version, samples)
		if why != "" {
			skip(name, why)
			continue
		}
		found := instances(r, samples)
		if len(found) == 0 {
			skip(name, "not in this node's FTDC: "+missingPath(r, samples, ""))
			continue
		}
		described, missing, thin := 0, "", ""
		for _, instance := range found {
			if gone := missingPath(r, samples, instance); gone != "" {
				missing = gone
				continue
			}
			seconds := Seconds(r, samples, instance, from, to)
			minutes := Minutes(r, seconds)
			if len(minutes) < minMinutes {
				thin = tooThin(r, seconds)
				continue
			}
			result := describe(r, seconds, minutes)
			result.Source, result.Instance, result.Version = source, instance, version
			result.paths = resolvedPaths(r, samples, instance)
			out = append(out, result)
			described++
		}
		switch {
		case described > 0:
		case missing != "":
			skip(name, "not in this node's FTDC: "+missing)
		default:
			skip(name, thin)
		}
	}
	return out, skipped
}

// tooThin says why a label had too few minutes to describe. A ratio whose divisor never moved — no
// reads to average a latency over, a standalone with no replication buffer — is a different answer
// from a window that is simply too short, and the reader should not have to guess which.
func tooThin(r Rule, seconds []Second) string {
	if len(seconds) == 0 {
		return "no samples in this window"
	}
	if len(r.Signal.Den) > 0 {
		what := "above zero"
		if r.Signal.MinDen > 0 {
			what = fmt.Sprintf("to %g or more", r.Signal.MinDen)
		}
		return fmt.Sprintf("fewer than %d minutes where %s summed %s: too little of it to divide by", minMinutes, strings.Join(r.Signal.Den, " + "), what)
	}
	return fmt.Sprintf("fewer than %d full minutes in this window", minMinutes)
}

// Choose is the definition of a label for a node. The node's version picks it when FTDC recorded
// one; without a version, the first definition whose metrics are all present is the one the node
// was evidently running. The reason is empty when one was found.
func Choose(name, version string, samples *ftdc.Samples) (Rule, string) {
	definitions := Definitions(name)
	if len(definitions) == 0 {
		return Rule{}, "no such label"
	}
	if parseVersion(version) != nil {
		covered := make([]string, 0, len(definitions))
		for _, r := range definitions {
			if ok, _ := r.applies(version); ok {
				return r, ""
			}
			covered = append(covered, r.VersionText())
		}
		return Rule{}, fmt.Sprintf("defined for %s; this node runs %s", strings.Join(covered, ", "), version)
	}
	for _, r := range definitions {
		if len(instances(r, samples)) > 0 && missingPath(r, samples, "") == "" {
			return r, ""
		}
	}
	return definitions[0], ""
}

// low reports whether the label's trouble is a low value.
func low(r Rule) bool { return r.Direction == "down" }

// describe reads the levels, the shape and the duty cycle off one label's minutes.
func describe(r Rule, seconds []Second, minutes []Minute) Result {
	series := make([]point, len(minutes))
	values := make([]float64, len(minutes))
	for i, m := range minutes {
		series[i] = point{m.At, m.Value(r)}
		values[i] = series[i].value
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	res := Result{
		Label: r.Name, Title: r.Title, Unit: r.Unit(), View: r.View, SignalUnit: r.Signal.Unit,
		Minutes: len(minutes), Typical: quantile(sorted, 0.5), P95: quantile(sorted, 0.95),
		rule: r, minutes: minutes,
	}
	worst := sorted[len(sorted)-1]
	if low(r) {
		worst = sorted[0]
	}
	res.Peak = worst
	for _, p := range series {
		if p.value == worst {
			res.PeakAt = p.at
			break
		}
	}
	res.PeakSecond, res.PeakSecondAt = extremeSecond(seconds, low(r) || r.View == ViewTimeBelow)
	if r.Timed() {
		for _, m := range minutes {
			res.PastSeconds += m.Past * float64(m.Samples) / 60
		}
		res.PastSeconds = math.Round(res.PastSeconds)
	}
	if len(r.Levels) > 0 {
		res.Level, res.PeakLevel = r.degrees(res.Typical), r.degrees(res.Peak)
	}
	res.Shape, res.Change, res.Episodes, res.Spikes, res.Periodic = shape(r, series, sorted)
	if r.Elevated != nil {
		d := duty(r, values)
		res.Duty = &d
	}
	return res
}

// extremeSecond is the highest second, or the lowest, and when it was.
func extremeSecond(seconds []Second, lowest bool) (float64, time.Time) {
	best, at := math.NaN(), time.Time{}
	for _, s := range seconds {
		if math.IsNaN(s.Value) {
			continue
		}
		if math.IsNaN(best) || (lowest && s.Value < best) || (!lowest && s.Value > best) {
			best, at = s.Value, s.At
		}
	}
	if math.IsNaN(best) {
		return 0, time.Time{}
	}
	return best, at
}

// duty is the share of minutes at or past the elevated mark, in words.
func duty(r Rule, values []float64) Duty {
	mark := *r.Elevated
	past := 0
	for _, v := range values {
		if (low(r) && v <= mark) || (!low(r) && v >= mark) {
			past++
		}
	}
	share := float64(past) / float64(len(values))
	label := "always"
	switch {
	case past == 0:
		label = "never"
	case share < 0.05:
		label = "rarely"
	case share < 0.5:
		label = "intermittently"
	case share < 0.95:
		label = "mostly"
	}
	return Duty{Mark: mark, Share: math.Round(share*1000) / 1000, Label: label}
}

// point is one minute's view value: what the shape is read on.
type point struct {
	at    time.Time
	value float64
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[max(0, int(math.Ceil(q*float64(len(sorted))))-1)]
}

// robustScale is the spread of values that one outlier cannot move: the median absolute deviation,
// scaled to read like a standard deviation.
func robustScale(sorted []float64) float64 {
	middle := quantile(sorted, 0.5)
	deviations := make([]float64, len(sorted))
	for i, v := range sorted {
		deviations[i] = math.Abs(v - middle)
	}
	sort.Float64s(deviations)
	return 1.4826 * quantile(deviations, 0.5)
}
