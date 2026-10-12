package label

import (
	"math"
	"sort"
)

// Related is another label on the same node that moved with a notable one.
//
// It is correlation, not cause: two series that rise together may share a cause neither of them
// is. What makes it useful is the lead — a label that moved first is a better candidate for the
// cause than one that followed — and that it is measured on the node's own minutes, so the
// question "what else changed at 12:00" is answered from the data rather than from a guess.
type Related struct {
	Label    string  `json:"label"`
	Instance string  `json:"instance,omitempty"`
	R        float64 `json:"r"` // Pearson correlation at the best lag; negative moves opposite
	// Lead is how many minutes earlier the related label moved: positive means it went first.
	Lead int `json:"lead_minutes"`
}

const (
	relateLag     = 10  // minutes either way a related label may lead or follow
	relateOverlap = 30  // minutes two series must share before a correlation means anything
	relateMinimum = 0.6 // |r| below this is not offered
	relateKeep    = 5
)

// notable is a shape that says something happened, which is what is worth relating.
func notable(r Result) bool {
	switch r.Shape {
	case "episodic", "periodic", "step_up", "step_down", "rising", "falling":
		return true
	}
	return false
}

// Relate fills Related on every notable result from the other results of the same node. Two labels
// read from a shared counter are never related to each other: disk await and disk read await
// moving together is arithmetic, not evidence.
func Relate(results []Result) {
	for i := range results {
		if !notable(results[i]) {
			continue
		}
		target := byMinute(results[i])
		found := make([]Related, 0, 8)
		for j := range results {
			if i == j || results[j].Source != results[i].Source || sharePath(results[i], results[j]) {
				continue
			}
			other := byMinute(results[j])
			r, lead := bestLag(target, other)
			if math.Abs(r) >= relateMinimum {
				found = append(found, Related{Label: results[j].Label, Instance: results[j].Instance,
					R: math.Round(r*100) / 100, Lead: lead})
			}
		}
		sort.SliceStable(found, func(a, b int) bool { return math.Abs(found[a].R) > math.Abs(found[b].R) })
		if len(found) > relateKeep {
			found = found[:relateKeep]
		}
		results[i].Related = found
	}
}

func sharePath(a, b Result) bool {
	for _, p := range a.paths {
		for _, q := range b.paths {
			if p == q {
				return true
			}
		}
	}
	return false
}

// byMinute is a result's view values keyed by minute.
func byMinute(r Result) map[int64]float64 {
	out := make(map[int64]float64, len(r.minutes))
	for _, m := range r.minutes {
		out[m.At.Unix()/60] = m.Value(r.rule)
	}
	return out
}

// bestLag is the strongest correlation between target at t and other at t-lag, over lags of up
// to relateLag minutes, with the lag it was found at. A tie goes to the smaller lag, so a pair that
// moved together is not reported as one leading the other.
func bestLag(target, other map[int64]float64) (float64, int) {
	best, at := 0.0, 0
	try := func(lag int) {
		r, ok := pearson(target, other, int64(lag))
		if ok && math.Abs(r) > math.Abs(best)+1e-9 {
			best, at = r, lag
		}
	}
	try(0)
	// Most pairs are unrelated; the lags are only worth the work when the two move together at all.
	if math.Abs(best) < relateMinimum/2 {
		return best, 0
	}
	for lag := 1; lag <= relateLag; lag++ {
		try(lag)
		try(-lag)
	}
	return best, at
}

func pearson(target, other map[int64]float64, lag int64) (float64, bool) {
	var n, sx, sy, sxx, syy, sxy float64
	for key, x := range target {
		y, ok := other[key-lag]
		if !ok {
			continue
		}
		n++
		sx, sy = sx+x, sy+y
		sxx, syy, sxy = sxx+x*x, syy+y*y, sxy+x*y
	}
	if n < relateOverlap {
		return 0, false
	}
	vx, vy := sxx-sx*sx/n, syy-sy*sy/n
	if vx <= 0 || vy <= 0 {
		return 0, false
	}
	return (sxy - sx*sy/n) / math.Sqrt(vx*vy), true
}

// Imbalance is one label compared across the things that should carry it alike: the disks of one
// node, or the nodes of a deployment. An average hides one hot disk in eight, and a deployment's
// totals hide the one shard taking every write.
type Imbalance struct {
	Label string `json:"label"`
	// Source is the node whose instances were compared; empty when the nodes themselves were.
	Source string `json:"source,omitempty"`
	// Peers is what the compared nodes had in common, when nodes were compared: their process,
	// cluster role and replica set state, such as "mongod (shard) SECONDARY". Empty when the bundle
	// did not say, and then only nodes that said nothing were compared.
	Peers   string  `json:"peers,omitempty"`
	Hottest string  `json:"hottest"`
	Value   float64 `json:"value"`  // the hottest one's p95
	Others  float64 `json:"others"` // the mean p95 of the rest
	Ratio   float64 `json:"ratio"`
	Count   int     `json:"count"`
	// Comparison says which fixed ratio the highest reached against the mean of the rest:
	// under_1.5x, 1.5x_or_more, 3x_or_more, or share_80pct_or_more (one carries 80% or more of the
	// total among three or more). It is arithmetic on p95s, not a judgement of the imbalance.
	Comparison string `json:"comparison"`
}

// PeerChanged is the peer of a node whose replica set state changed within the window: it was a
// primary part of the time and something else the rest, so it has no peers to be compared with.
const PeerChanged = "changed state"

// ComparisonUnder is an Imbalance whose highest member stayed under 1.5x the rest.
const ComparisonUnder = "under_1.5x"

// Imbalances compares each label's p95 across a node's instances, and across nodes for labels that
// are one per node. A label every member idles on is not compared: one disk at 2% busy against
// another at 0.5% is four times as busy and means nothing.
//
// Nodes are compared only with peers in the same role, as peers says per source: a primary takes
// the writes and its secondaries apply them, so a primary busier than its secondaries is what a
// replica set is, not one node out of line; a config server is not a shard. A source missing from
// peers is compared with the other sources missing from it, and one whose peer is PeerChanged with
// nothing.
func Imbalances(results []Result, peers map[string]string) []Imbalance {
	type key struct{ label, source, peers string }
	groups := map[key][]Result{}
	order := make([]key, 0, 16)
	for _, r := range results {
		if r.rule.Signal.Behind {
			continue // every node reports every member's lag; comparing them compares the same thing
		}
		k := key{label: r.Label}
		if r.Instance != "" {
			k.source = r.Source
		} else if k.peers = peers[r.Source]; k.peers == PeerChanged {
			continue
		}
		if groups[k] == nil {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	out := make([]Imbalance, 0, len(order))
	for _, k := range order {
		members := groups[k]
		if len(members) < 2 {
			continue
		}
		c := members[0].rule
		sort.SliceStable(members, func(a, b int) bool { return members[a].P95 > members[b].P95 })
		hottest := members[0]
		if hottest.P95 < c.MinChange {
			continue
		}
		total, rest := 0.0, 0.0
		for _, m := range members {
			total += m.P95
		}
		rest = (total - hottest.P95) / float64(len(members)-1)
		ratio := hottest.P95 / math.Max(rest, c.MinChange/10)
		comparison := ComparisonUnder
		switch {
		case len(members) >= 3 && total > 0 && hottest.P95/total >= 0.8:
			comparison = "share_80pct_or_more"
		case ratio >= 3:
			comparison = "3x_or_more"
		case ratio >= 1.5:
			comparison = "1.5x_or_more"
		}
		name := hottest.Source
		if k.source != "" {
			name = hottest.Instance
		}
		out = append(out, Imbalance{Label: k.label, Source: k.source, Peers: k.peers, Hottest: name,
			Value: hottest.P95, Others: rest, Ratio: math.Round(ratio*10) / 10, Count: len(members), Comparison: comparison})
	}
	return out
}
