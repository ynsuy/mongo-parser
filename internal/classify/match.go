package classify

import (
	"time"

	"github.com/ynsuy/mongo-parser/internal/label"
)

// Match is one classifier found true for one node.
type Match struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Doc    string `json:"doc"`
	Source string `json:"source"`
	// Because is which label, and instance when it has one, met each of the classifier's
	// conditions, and at what.
	Because []Evidence `json:"because"`
}

// Evidence is the label that met one of a classifier's conditions, and how: the window it was read
// over travels with it, so the match does not have to be read beside the label a second time to
// know when, or how far past its usual level.
type Evidence struct {
	Label    string `json:"label"`
	Instance string `json:"instance,omitempty"`
	Unit     string `json:"unit"`
	Band     string `json:"band"`
	// Usual is the label's own median minute over the window it was read, and P95 its 95th
	// percentile minute; Peak is its worst minute, and PeakAt when.
	Usual  float64   `json:"usual"`
	P95    float64   `json:"p95"`
	Peak   float64   `json:"peak"`
	PeakAt time.Time `json:"peak_at"`
	// Next is the command that opens this label up, filled in by whoever has the bundle path and
	// the flags; empty until then.
	Next string `json:"next,omitempty"`
}

// Classify finds every classifier that matches, node by node: every one of its conditions met at
// once, on the same instance when the conditions name instanced labels — disk.busy and disk.await
// must agree on which disk, not each pick whichever device suits it.
func Classify(results []label.Result) []Match {
	bySource := map[string][]label.Result{}
	order := make([]string, 0, 8)
	for _, r := range results {
		if bySource[r.Source] == nil {
			order = append(order, r.Source)
		}
		bySource[r.Source] = append(bySource[r.Source], r)
	}
	out := make([]Match, 0, 8)
	for _, source := range order {
		for _, c := range Classifiers {
			for _, m := range evaluate(c, bySource[source]) {
				m.Source = source
				out = append(out, m)
			}
		}
	}
	return out
}

// evaluate is every way a classifier matches a node: once for each instance that satisfies every
// instanced condition together, or once, instance-less, when none of its conditions are instanced.
// Two disks that are each, on their own, the whole pattern are two matches, not one blended from
// both.
func evaluate(c Classifier, node []label.Result) []Match {
	instances := candidateInstances(c, node)
	if len(instances) == 0 {
		instances = []string{""}
	}
	out := make([]Match, 0, 1)
	for _, instance := range instances {
		because := make([]Evidence, 0, len(c.When))
		ok := true
		for _, w := range c.When {
			e, found := worstMeeting(node, w, instance)
			if !found {
				ok = false
				break
			}
			because = append(because, e)
		}
		if ok {
			out = append(out, Match{Name: c.Name, Title: c.Title, Doc: c.Doc, Because: because})
		}
	}
	return out
}

// candidateInstances is every instance worth trying a classifier's conditions against: every
// distinct instance among the results of the labels it names. Empty when none of them have
// instances of their own.
func candidateInstances(c Classifier, node []label.Result) []string {
	names := make(map[string]bool, len(c.When))
	for _, w := range c.When {
		names[w.Label] = true
	}
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	for _, r := range node {
		if names[r.Label] && r.Instance != "" && !seen[r.Instance] {
			seen[r.Instance] = true
			out = append(out, r.Instance)
		}
	}
	return out
}

// worstMeeting is the result of the condition's label that meets it most, when any does: the
// highest peak, or the lowest for a label whose trouble is a low value. instance, when set, is the
// only instance considered; a label with no instances of its own is read regardless of it.
func worstMeeting(node []label.Result, w Condition, instance string) (Evidence, bool) {
	var best *label.Result
	for i := range node {
		r := &node[i]
		if r.Label != w.Label || !r.Rule().AtLeast(r.Peak, w.AtLeast) {
			continue
		}
		if r.Instance != "" && instance != "" && r.Instance != instance {
			continue
		}
		if best == nil || moreExtreme(*r, *best) {
			best = r
		}
	}
	if best == nil {
		return Evidence{}, false
	}
	band, _ := best.Rule().Strongest(best.Peak)
	return Evidence{Label: best.Label, Instance: best.Instance, Unit: best.Unit, Band: band,
		Usual: best.Typical, P95: best.P95, Peak: best.Peak, PeakAt: best.PeakAt}, true
}

func moreExtreme(a, b label.Result) bool {
	if a.Rule().Direction == "down" {
		return a.Peak < b.Peak
	}
	return a.Peak > b.Peak
}
