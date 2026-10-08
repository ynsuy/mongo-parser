package funnel

import (
	"strings"
	"testing"
	"time"

	"mongo-log-parser/internal/ftdc"
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

// A device busy every second that still answers in a fraction of a millisecond is noticed, not
// opened: disk.busy needs disk.await to agree, and here it stayed fast.
func TestABusyDiskThatAnswersFastIsNotWorthOpening(t *testing.T) {
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
	stall := Lay(results, nil, nil, func(label.Result) string { return "" })
	for _, item := range stall.Open {
		if item.Label == "disk.busy" {
			t.Fatalf("a fast, busy disk is on the open list: %+v", item.Why)
		}
	}
	for _, item := range stall.Notice {
		if item.Label == "disk.busy" {
			if why := strings.Join(item.Why, "; "); !strings.Contains(why, "disk.await stayed fast") {
				t.Fatalf("why = %q", why)
			}
			return
		}
	}
	t.Fatal("disk.busy is not on the notice list")
}
