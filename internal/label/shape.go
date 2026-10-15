package label

import (
	"math"
	"sort"
	"time"
)

// The shape of a series is read in a fixed order, most specific first, so one label wins:
//
//   - constant:    it never moved.
//   - rising, falling: it moved one way across the whole window.
//   - step_up, step_down: it sat at one level, then at another, and stayed.
//   - episodic:    it rose clearly above its own usual level for three minutes or more, then came
//     back — the shape of an incident, and the one a fixed threshold misses when the level it
//     rose to is high for this server but not high for every server.
//   - periodic:    its episodes or spikes came back at a steady interval — every hour at :05, every
//     ten minutes — which is a job, not an accident.
//   - spiky:       it went up and came straight back, for less than three minutes at a time.
//   - fluctuating, steady: it moved about, a lot or a little.
//
// Every test asks for a change of at least the rule's min_change, in its own unit, so a disk
// going from 1% to 3% busy is steady, not tripled.

const (
	episodeMinutes = 3 // the shortest excursion that is an episode rather than a spike
	episodeGap     = 2 // minutes back at baseline that do not end an episode
	maxEpisodes    = 3
)

func shape(c Rule, series []point, sorted []float64) (string, *Change, []Episode, int, *Periodic) {
	if sorted[0] == sorted[len(sorted)-1] {
		return "constant", nil, nil, 0, nil
	}
	baseline, scale := quantile(sorted, 0.5), robustScale(sorted)
	episodes, spikes, starts := excursions(c, series, baseline, scale)
	recurring := periodic(starts)

	if direction := trend(c, series); direction != "" {
		return direction, nil, episodes, spikes, recurring
	}
	if change := step(c, series); change != nil {
		label := "step_up"
		if change.After < change.Before {
			label = "step_down"
		}
		return label, change, episodes, spikes, recurring
	}
	if recurring != nil {
		return "periodic", nil, episodes, spikes, recurring
	}
	if len(episodes) > 0 {
		return "episodic", nil, episodes, spikes, nil
	}
	if spikes > 0 {
		return "spiky", nil, nil, spikes, nil
	}
	if scale >= math.Max(c.MinChange, 0.25*math.Abs(baseline)) {
		return "fluctuating", nil, nil, 0, nil
	}
	return "steady", nil, nil, 0, nil
}

// periodic finds a steady interval between excursion starts: at least four occurrences, at least
// two minutes apart, and four in five gaps within a tenth of the typical one (or two minutes,
// whichever is wider). A missed occurrence — a job that skipped a run, an episode too small to
// clear the margin — counts as two gaps of the interval rather than breaking it.
func periodic(starts []time.Time) *Periodic {
	if len(starts) < 4 {
		return nil
	}
	gaps := make([]float64, 0, len(starts)-1)
	for i := 1; i < len(starts); i++ {
		gaps = append(gaps, starts[i].Sub(starts[i-1]).Minutes())
	}
	sorted := append([]float64(nil), gaps...)
	sort.Float64s(sorted)
	every := quantile(sorted, 0.5)
	if every < 2 {
		return nil
	}
	tolerance := math.Max(2, 0.1*every)
	fit := 0
	for _, gap := range gaps {
		multiple := math.Max(1, math.Round(gap/every))
		if math.Abs(gap-multiple*every) <= tolerance {
			fit++
		}
	}
	if float64(fit) < 0.8*float64(len(gaps)) {
		return nil
	}
	return &Periodic{Every: time.Duration(math.Round(every)) * time.Minute, Occurrences: len(starts),
		First: starts[0], Last: starts[len(starts)-1]}
}

// margin is how far above the usual level a minute must be to count as elevated: three robust
// deviations, half the usual level, and the rule's own minimum, whichever is largest. The
// half keeps a metric that is usually 10 from calling 14 an incident; the minimum keeps a metric
// that is usually 0 from calling 0.1 one.
func margin(c Rule, baseline, scale float64) float64 {
	return math.Max(math.Max(3*scale, 0.5*math.Abs(baseline)), c.MinChange)
}

// excursions finds the stretches above the usual level — below it for a rule whose direction
// is down — the long ones as episodes, the short ones counted as spikes, and when every one of
// them began.
func excursions(c Rule, series []point, baseline, scale float64) ([]Episode, int, []time.Time) {
	if c.Direction == "down" {
		flipped := make([]point, len(series))
		for i, m := range series {
			flipped[i] = point{m.at, -m.value}
		}
		episodes, spikes, starts := excursions(Rule{MinChange: c.MinChange}, flipped, -baseline, scale)
		for i := range episodes {
			episodes[i].Mean, episodes[i].Peak, episodes[i].Baseline = -episodes[i].Mean, -episodes[i].Peak, -episodes[i].Baseline
		}
		return episodes, spikes, starts
	}
	threshold := baseline + margin(c, baseline, scale)
	type run struct{ from, to int } // indices, inclusive
	runs := make([]run, 0, 8)
	for i, m := range series {
		if m.value <= threshold {
			continue
		}
		// Contiguous in time, allowing a short dip back, joins the run before it.
		if n := len(runs); n > 0 && m.at.Sub(series[runs[n-1].to].at) <= time.Duration(episodeGap+1)*time.Minute {
			runs[n-1].to = i
			continue
		}
		runs = append(runs, run{i, i})
	}
	episodes, spikes := make([]Episode, 0, len(runs)), 0
	starts := make([]time.Time, 0, len(runs))
	last := series[len(series)-1].at
	for _, r := range runs {
		starts = append(starts, series[r.from].at)
		minutes := int(series[r.to].at.Sub(series[r.from].at)/time.Minute) + 1
		if minutes < episodeMinutes {
			spikes++
			continue
		}
		e := Episode{Start: series[r.from].at, End: series[r.to].at.Add(time.Minute), Minutes: minutes,
			Baseline: baseline, Open: series[r.to].at.Equal(last)}
		sum := 0.0
		for _, m := range series[r.from : r.to+1] {
			sum += m.value
			e.Peak = math.Max(e.Peak, m.value)
		}
		e.Mean = sum / float64(r.to-r.from+1)
		episodes = append(episodes, e)
	}
	// The ones that weigh most first: how long, times how far above the usual level.
	weight := func(e Episode) float64 { return float64(e.Minutes) * (e.Mean - e.Baseline) }
	for i := 1; i < len(episodes); i++ {
		for j := i; j > 0 && weight(episodes[j]) > weight(episodes[j-1]); j-- {
			episodes[j], episodes[j-1] = episodes[j-1], episodes[j]
		}
	}
	if len(episodes) > maxEpisodes {
		episodes = episodes[:maxEpisodes]
	}
	return episodes, spikes, starts
}

// trendPoints caps the series a trend is measured on; Kendall's tau compares every pair.
const trendPoints = 240

// trend is rising or falling when the series moved one way across the whole window: Kendall's tau
// strongly one-signed, and the last tenth clearly away from the first. Empty otherwise.
func trend(c Rule, series []point) string {
	if len(series) < 3*minMinutes {
		return ""
	}
	step := max(1, len(series)/trendPoints)
	values := make([]float64, 0, trendPoints+1)
	for i := 0; i < len(series); i += step {
		values = append(values, series[i].value)
	}
	concordant, discordant := 0, 0
	for i := range values {
		for j := i + 1; j < len(values); j++ {
			switch {
			case values[j] > values[i]:
				concordant++
			case values[j] < values[i]:
				discordant++
			}
		}
	}
	pairs := len(values) * (len(values) - 1) / 2
	tau := float64(concordant-discordant) / float64(pairs)
	tenth := max(1, len(values)/10)
	first, last := mean(values[:tenth]), mean(values[len(values)-tenth:])
	if math.Abs(last-first) < c.MinChange || math.Abs(last-first) < 0.5*math.Min(math.Abs(first), math.Abs(last)) {
		return ""
	}
	switch {
	case tau >= 0.6:
		return "rising"
	case tau <= -0.6:
		return "falling"
	}
	return ""
}

// step finds the one moment that best splits the series into two flat stretches, and keeps it when
// the two levels are far apart against the noise inside each and both stretches are long enough to
// be levels rather than an episode.
func step(c Rule, series []point) *Change {
	n := len(series)
	shortest := max(minMinutes, n/5)
	if n < 2*shortest {
		return nil
	}
	prefix, square := make([]float64, n+1), make([]float64, n+1)
	for i, m := range series {
		prefix[i+1] = prefix[i] + m.value
		square[i+1] = square[i] + m.value*m.value
	}
	cost := func(from, to int) float64 { // squared error of [from, to) about its mean
		k := float64(to - from)
		s := prefix[to] - prefix[from]
		return square[to] - square[from] - s*s/k
	}
	best, at := math.Inf(1), -1
	for i := shortest; i <= n-shortest; i++ {
		if total := cost(0, i) + cost(i, n); total < best {
			best, at = total, i
		}
	}
	if at < 0 {
		return nil
	}
	before := (prefix[at]) / float64(at)
	after := (prefix[n] - prefix[at]) / float64(n-at)
	noise := math.Sqrt(best / float64(n))
	shift := math.Abs(after - before)
	if shift < c.MinChange || shift < 4*noise || shift < 0.5*math.Min(math.Abs(before), math.Abs(after)) {
		return nil
	}
	return &Change{At: series[at].at, Before: before, After: after}
}

func mean(values []float64) float64 {
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}
