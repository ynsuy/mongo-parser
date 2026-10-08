package funnel

import (
	"testing"

	"mongo-log-parser/internal/label"
)

// results reads labels off a run of synthetic samples, so the tiers are tested against the rules
// the binary ships rather than against hand-made results that could drift from them.
func TestTheStallSortsByWhatTheRulesSay(t *testing.T) {
	results := run(t)
	stall := Lay(results, nil, nil, func(r label.Result) string { return "label " + r.Label })
	where := map[string]string{}
	for _, item := range stall.Open {
		where[item.Label] = TierOpen
	}
	for _, item := range stall.Notice {
		where[item.Label] = TierNotice
	}
	// Ten seconds without a ticket is worth opening, however short.
	if where["tickets.read.empty"] != TierOpen || where["tickets.read.scarce"] != TierOpen {
		t.Errorf("an emptied pool is not on the open list: %v", where)
	}
	// A pool half used all along is worth noticing, not opening.
	if where["tickets.write.used"] != TierNotice {
		t.Errorf("a half-used pool is %q, want notice", where["tickets.write.used"])
	}
	// A pool that sat at a quarter and never moved is ordinary.
	if _, listed := where["tickets.write.pool"]; listed {
		t.Errorf("an unchanging pool size was listed: %v", where)
	}
	ordinary := 0
	for _, n := range stall.Ordinary {
		ordinary += n.Count
	}
	if ordinary == 0 {
		t.Error("nothing was ordinary")
	}
	if stall.Open[0].Next != "label "+stall.Open[0].Label {
		t.Errorf("an open item names the command that opens it: %q", stall.Open[0].Next)
	}
}
