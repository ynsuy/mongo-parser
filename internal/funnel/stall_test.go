package funnel

import (
	"testing"
	"time"

	"github.com/ynsuy/mongo-parser/internal/classify"
	"github.com/ynsuy/mongo-parser/internal/ftdc"
	"github.com/ynsuy/mongo-parser/internal/label"
)

// results reads labels off a run of synthetic samples, so the tiers are tested against the rules
// the binary ships rather than against hand-made results that could drift from them.
func TestTheStallSortsByWhatTheRulesSay(t *testing.T) {
	results := run(t)
	stall := Lay(results, nil, nil, nil, func(r label.Result) string { return "label " + r.Label })
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

// A shape-only label — no bands of its own — that bursts to twenty times its usual rate for five
// minutes is on the open list: the shape is the only signal it can give, not a lesser one.
func TestAShapeOnlyLabelsEpisodeIsWorthOpening(t *testing.T) {
	seconds := 2 * 3600
	s := &ftdc.Samples{Values: map[string][]float64{}, Kinds: map[string]string{}}
	for i := range seconds {
		s.Timestamps = append(s.Timestamps, start.Add(time.Duration(i)*time.Second).UnixMilli())
	}
	burst := func(i int) bool { m := i / 60; return m >= 20 && m < 25 }
	for _, p := range []string{"query", "getmore", "insert", "update", "delete", "command"} {
		set := make([]float64, seconds)
		total := 0.0
		for i := range set {
			rate := 100.0 / 6
			if burst(i) {
				rate = 2000.0 / 6
			}
			total += rate
			set[i] = total
		}
		s.Values["serverStatus/opcounters/"+p] = set
	}
	results, _ := label.Run(s, "n", "", time.Time{}, time.Time{})
	stall := Lay(results, nil, nil, nil, func(label.Result) string { return "" })
	for _, item := range stall.Open {
		if item.Label == "ops.total" {
			return
		}
	}
	for _, item := range stall.Notice {
		if item.Label == "ops.total" {
			t.Fatalf("a bandless label's episode belongs on the open list, not notice: %+v", item.Why)
		}
	}
	t.Fatal("ops.total is not on the open list")
}

// A device busy every second that still answers in a fraction of a millisecond is still on the open
// list — a label's own tier reads only its own band, nothing else corroborates it — but it does not
// classify as io_saturation, since disk.await never left its lowest band: the corroboration disk.busy
// used to need from disk.await now lives in package classify, not in the stall's own tiering.
func TestABusyDiskThatAnswersFastIsOpenButNotClassified(t *testing.T) {
	d := "systemMetrics/disks/nvme1n1/"
	seconds := 2 * 3600
	s := &ftdc.Samples{Values: map[string][]float64{}, Kinds: map[string]string{}}
	for i := range seconds {
		s.Timestamps = append(s.Timestamps, start.Add(time.Duration(i)*time.Second).UnixMilli())
	}
	count := func(path string, rate float64) {
		values := make([]float64, seconds)
		for i := range values {
			values[i] = rate * float64(i)
		}
		s.Values[path] = values
	}
	count(d+"io_time_ms", 990) // busy 99% of every second
	count(d+"reads", 5000)     // five thousand reads a second
	count(d+"writes", 0)
	count(d+"read_time_ms", 5000*0.1) // at a tenth of a millisecond each
	count(d+"write_time_ms", 0)
	results, _ := label.Run(s, "n", "", time.Time{}, time.Time{})
	stall := Lay(results, nil, classify.Classify(results), nil, func(label.Result) string { return "" })
	found := false
	for _, item := range stall.Open {
		if item.Label == "disk.busy" {
			found = true
		}
	}
	if !found {
		t.Fatal("a busy disk's own reading should still be on the open list")
	}
	for _, m := range stall.Classified {
		if m.Name == "io_saturation" {
			t.Fatalf("a fast, busy disk should not classify as io_saturation: %+v", m)
		}
	}
}

// The stall carries classified matches through untouched: laying out labels does not touch what
// package classify already decided.
func TestTheStallCarriesClassifiedMatchesThrough(t *testing.T) {
	results := run(t)
	given := []classify.Match{{Name: "io_saturation", Title: "I/O saturation", Source: "rs0-0"}}
	stall := Lay(results, nil, given, nil, func(label.Result) string { return "" })
	if len(stall.Classified) != 1 || stall.Classified[0].Name != "io_saturation" {
		t.Fatalf("%+v", stall.Classified)
	}
}

// A label named as a classifier's evidence is left off the open list — Classified already carries
// its reading and, now, the command that opens it — so it is not repeated under a second heading.
func TestALabelNamedAsEvidenceIsLeftOffTheOpenList(t *testing.T) {
	results := run(t)
	given := []classify.Match{{
		Name: "read_ticket_exhaustion", Source: "rs0-0",
		Because: []classify.Evidence{{Label: "tickets.read.empty", Band: "constant"}},
	}}
	stall := Lay(results, nil, given, nil, func(r label.Result) string { return "open " + r.Label })
	for _, item := range stall.Open {
		if item.Label == "tickets.read.empty" {
			t.Fatalf("tickets.read.empty is evidence for read_ticket_exhaustion; it should not also be open: %+v", item)
		}
	}
	if next := stall.Classified[0].Because[0].Next; next != "open tickets.read.empty" {
		t.Fatalf("the evidence should carry the command that opens it: %q", next)
	}
}
