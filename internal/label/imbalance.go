package label

import (
	"math"
	"sort"
)

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
