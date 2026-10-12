package funnel

import (
	"fmt"
	"sort"
	"strings"

	"mongo-log-parser/internal/label"
)

// The tiers a label can sit in.
const (
	TierOpen     = "open"     // worth opening up
	TierNotice   = "notice"   // worth noticing: above its lowest band, or its shape changed
	TierOrdinary = "ordinary" // sat in its lowest band and did nothing remarkable
)

// Item is one label on one node, and why it is where it is.
type Item struct {
	label.Result
	Tier string `json:"tier"`
	// Why is what put it in its tier, in the rule's own terms: "peak near_full", "episodic",
	// "past its mark mostly (60% of minutes)".
	Why []string `json:"why,omitempty"`
	// Next is the command that opens the label up.
	Next string `json:"next"`
	// rank orders the items within a tier: the band the peak reached, as a share of the way up the
	// rule's bands, then how much of the window was spent past the mark.
	rank float64
}

// NodeCount is how many of a node's labels were ordinary, and which.
type NodeCount struct {
	Source string   `json:"source"`
	Count  int      `json:"count"`
	Labels []string `json:"labels"`
}

// Stall is every label laid out for a first look.
type Stall struct {
	Open       []Item            `json:"open"`
	Notice     []Item            `json:"notice"`
	Ordinary   []NodeCount       `json:"ordinary"`
	Imbalances []label.Imbalance `json:"imbalances,omitempty"`
	Skipped    []label.Skip      `json:"skipped,omitempty"`
}

// notableShapes are the shapes that say something happened.
var notableShapes = map[string]bool{
	"episodic": true, "periodic": true, "step_up": true, "step_down": true, "rising": true, "falling": true,
}

// Lay sorts the labels into the stall. next makes the command that opens one; it is the caller's
// because the bundle path and the flags are.
func Lay(results []label.Result, imbalances []label.Imbalance, skipped []label.Skip, next func(label.Result) string) Stall {
	stall := Stall{Skipped: skipped}
	ordinary := map[string]*NodeCount{}
	order := make([]string, 0, 8)
	index := map[string]label.Result{}
	for _, r := range results {
		index[key(r.Source, r.Label, r.Instance)] = r
	}
	for _, r := range results {
		item := tier(r)
		corroborate(&item, index)
		item.Next = next(r)
		switch item.Tier {
		case TierOpen:
			stall.Open = append(stall.Open, item)
		case TierNotice:
			stall.Notice = append(stall.Notice, item)
		default:
			n := ordinary[r.Source]
			if n == nil {
				n = &NodeCount{Source: r.Source}
				ordinary[r.Source] = n
				order = append(order, r.Source)
			}
			n.Count++
			n.Labels = append(n.Labels, strings.TrimSpace(r.Label+" "+r.Instance))
		}
	}
	for _, items := range [][]Item{stall.Open, stall.Notice} {
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].rank != items[j].rank {
				return items[i].rank > items[j].rank
			}
			if items[i].Source != items[j].Source {
				return items[i].Source < items[j].Source
			}
			return items[i].Label < items[j].Label
		})
	}
	for _, source := range order {
		stall.Ordinary = append(stall.Ordinary, *ordinary[source])
	}
	for _, im := range imbalances {
		if im.Comparison != label.ComparisonUnder {
			stall.Imbalances = append(stall.Imbalances, im)
		}
	}
	return stall
}

func key(source, name, instance string) string { return source + "\x00" + name + "\x00" + instance }

// corroborate holds a label back from the open list when the labels its rule needs to agree all
// stayed in their lowest band on the same node and instance. It is still worth noticing: the
// reading is real, it is only not a reason to look further on its own.
func corroborate(item *Item, index map[string]label.Result) {
	rule := item.Rule()
	if item.Tier != TierOpen || len(rule.OpenWith) == 0 {
		return
	}
	quiet := make([]string, 0, len(rule.OpenWith))
	for _, name := range rule.OpenWith {
		other, found := index[key(item.Source, name, item.Instance)]
		if !found {
			return // nothing to disagree with: the label stands on its own
		}
		if _, band := other.Rule().Strongest(other.Peak); band > 0 {
			return
		}
		lowest, _ := other.Rule().Strongest(other.Peak)
		quiet = append(quiet, name+" stayed "+lowest)
	}
	item.Tier = TierNotice
	item.Why = append(item.Why, "but "+strings.Join(quiet, ", "))
}

// tier decides where one label sits, and records why.
//
// Open is a peak in a band the rule says is worth opening (the top two of four or more by default),
// or a mark the label spent more than a rare minute past. Notice is a peak anywhere above the
// lowest band, or a notable shape — a workload that stepped up is worth a glance even when no level
// is high, but a shape alone does not make a middle band worth opening.
func tier(r label.Result) Item {
	rule := r.Rule()
	item := Item{Result: r, Tier: TierOrdinary}
	band, index := rule.Strongest(r.Peak)
	open := rule.OpenFrom()
	shape := notableShapes[r.Shape]
	if len(rule.Levels) > 0 {
		item.rank = float64(index) / float64(len(rule.Levels)-1)
	}
	if r.Duty != nil {
		item.rank += r.Duty.Share / 10 // within a band, more of the window past the mark ranks higher
	}
	switch {
	case open >= 0 && index >= open:
		item.Tier = TierOpen
		item.Why = append(item.Why, "peak "+band)
	case index > 0:
		item.Tier = TierNotice
		item.Why = append(item.Why, "peak "+band)
	}
	if r.Duty != nil && r.Duty.Share >= 0.05 {
		item.Tier = TierOpen
		item.Why = append(item.Why, fmt.Sprintf("past its mark %s (%.0f%% of minutes)", r.Duty.Label, r.Duty.Share*100))
	}
	if shape {
		item.Why = append(item.Why, r.Shape)
		if item.Tier == TierOrdinary {
			item.Tier = TierNotice
		}
	}
	return item
}
