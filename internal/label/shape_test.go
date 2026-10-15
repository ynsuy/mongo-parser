package label

import (
	"math"
	"strings"
	"testing"
	"time"
)

// The case a fixed threshold misses: a CPU that usually sits at 10% climbs to 60% for ten minutes.
// Sixty percent is not high for a machine, and it is the whole story for this one.
func TestATenMinutePlateauIsAnEpisode(t *testing.T) {
	busy := func(m int) float64 {
		if m >= 60 && m < 70 {
			return 60
		}
		return 10
	}
	r := find(t, run(samplesFor(3*3600, cpuAt(busy)), "node-a"), "cpu.busy", "")
	if r.Shape != "episodic" || len(r.Episodes) != 1 {
		t.Fatalf("shape %s, episodes %+v", r.Shape, r.Episodes)
	}
	e := r.Episodes[0]
	if !e.Start.Equal(start.Add(60*time.Minute)) || e.Minutes < 10 || e.Minutes > 11 || math.Abs(e.Mean-60) > 3 {
		t.Fatalf("episode %+v, want 11:00 for ten minutes at about 60%%", e)
	}
	if r.Duty == nil || r.Duty.Label != "never" {
		t.Errorf("60%% never crossed the 80%% mark, duty says %+v", r.Duty)
	}
}

func TestAStepIsALevelThatStays(t *testing.T) {
	busy := func(m int) float64 {
		if m >= 90 {
			return 55
		}
		return 15
	}
	r := find(t, run(samplesFor(3*3600, cpuAt(busy)), "node-a"), "cpu.busy", "")
	if r.Shape != "step_up" || r.Change == nil || !r.Change.At.Equal(start.Add(90*time.Minute)) {
		t.Fatalf("shape %s change %+v, want a step up at 11:30", r.Shape, r.Change)
	}
}

func TestARampIsRising(t *testing.T) {
	r := find(t, run(samplesFor(3*3600, cpuAt(func(m int) float64 { return 10 + float64(m)*0.4 })), "node-a"), "cpu.busy", "")
	if r.Shape != "rising" {
		t.Fatalf("shape %s, want rising", r.Shape)
	}
}

func TestSmallWobbleIsSteadyAndOneMinuteIsASpike(t *testing.T) {
	wobble := func(m int) float64 { return 20 + float64(m%3) }
	if r := find(t, run(samplesFor(2*3600, cpuAt(wobble)), "n"), "cpu.busy", ""); r.Shape != "steady" {
		t.Fatalf("20–22%% is %s, want steady", r.Shape)
	}
	spike := func(m int) float64 {
		if m == 40 {
			return 90
		}
		return 20
	}
	r := find(t, run(samplesFor(2*3600, cpuAt(spike)), "n"), "cpu.busy", "")
	if r.Shape != "spiky" || r.Spikes != 1 || r.PeakLevel["very_high"] == 0 {
		t.Fatalf("one minute at 90%%: shape %s spikes %d peak level %v", r.Shape, r.Spikes, r.PeakLevel)
	}
}

// A job that runs every hour for five minutes. Each occurrence looks like an episode on its own;
// together they are a schedule.
func TestAnHourlyJobIsPeriodic(t *testing.T) {
	busy := func(m int) float64 {
		if m%60 >= 5 && m%60 < 10 {
			return 70
		}
		return 15
	}
	r := find(t, run(samplesFor(8*3600, cpuAt(busy)), "n"), "cpu.busy", "")
	if r.Shape != "periodic" || r.Periodic == nil {
		t.Fatalf("shape %s periodic %+v", r.Shape, r.Periodic)
	}
	if r.Periodic.Every != time.Hour || r.Periodic.Occurrences != 8 || !r.Periodic.First.Equal(start.Add(5*time.Minute)) {
		t.Fatalf("periodic %+v, want every hour from 10:05, eight times", r.Periodic)
	}
}

// Three bursts at uneven times are three bursts, not a schedule.
func TestIrregularBurstsAreNotPeriodic(t *testing.T) {
	at := map[int]bool{30: true, 31: true, 32: true, 95: true, 96: true, 97: true, 250: true, 251: true, 252: true, 400: true, 401: true, 402: true}
	busy := func(m int) float64 {
		if at[m] {
			return 70
		}
		return 15
	}
	r := find(t, run(samplesFor(8*3600, cpuAt(busy)), "n"), "cpu.busy", "")
	if r.Periodic != nil {
		t.Fatalf("bursts at 30, 95, 250 and 400 minutes read as periodic: %+v", r.Periodic)
	}
}

// Disks are labelled one by one, and a loopback is not a disk.
func TestDisksAreInstancesAndVirtualOnesAreSkipped(t *testing.T) {
	d := "systemMetrics/disks/"
	paths := map[string]func(int) float64{}
	for _, dev := range []string{"nvme0n1", "loop0"} {
		paths[d+dev+"/io_time_ms"] = counter(func(int) float64 { return 300 })
		paths[d+dev+"/reads"] = counter(func(int) float64 { return 50 })
		paths[d+dev+"/writes"] = counter(func(int) float64 { return 50 })
		paths[d+dev+"/read_time_ms"] = counter(func(int) float64 { return 50 * 25 })
		paths[d+dev+"/write_time_ms"] = counter(func(int) float64 { return 50 * 25 })
	}
	results := run(samplesFor(1800, paths), "n")
	busy := find(t, results, "disk.busy", "nvme0n1")
	if math.Abs(busy.Typical-30) > 1 {
		t.Errorf("busy %.1f%%, want 30", busy.Typical)
	}
	await := find(t, results, "disk.await", "nvme0n1")
	if math.Abs(await.Typical-25) > 1 || await.Level["slow"] == 0 {
		t.Errorf("await %.1f ms %v, want 25 and slow", await.Typical, await.Level)
	}
	for _, r := range results {
		if r.Instance == "loop0" {
			t.Fatalf("a loopback was labelled: %+v", r)
		}
	}
}

// The heap outside the cache is one gauge less another.
func TestMinusSubtractsALevel(t *testing.T) {
	gib := 1073741824.0
	results := run(samplesFor(1800, map[string]func(int) float64{
		"serverStatus/tcmalloc/generic/current_allocated_bytes":      func(int) float64 { return 12 * gib },
		"serverStatus/wiredTiger/cache/bytes currently in the cache": func(int) float64 { return 8 * gib },
	}), "n")
	r := find(t, results, "memory.heap_outside_cache", "")
	if math.Abs(r.Typical-4) > 0.01 {
		t.Fatalf("heap outside the cache %.2f GiB, want 4", r.Typical)
	}
}

// Each member is read against the newest optime any member reports; an arbiter's zero is not lag.
func TestMemberLagIsBehindTheNewestMember(t *testing.T) {
	m := "replSetGetStatus/members/"
	base := float64(start.UnixMilli())
	results := run(samplesFor(1800, map[string]func(int) float64{
		m + "0/optimeDate": func(i int) float64 { return base + float64(i)*1000 },
		m + "1/optimeDate": func(i int) float64 { return base + float64(i)*1000 - 45000 },
		m + "2/optimeDate": func(int) float64 { return 0 },
	}), "n")
	if lag := find(t, results, "repl.member_lag", "1"); math.Abs(lag.Typical-45) > 0.1 || lag.Level["lagging"] == 0 {
		t.Fatalf("member 1 lag %.1f s %v, want 45 and lagging", lag.Typical, lag.Level)
	}
	if lag := find(t, results, "repl.member_lag", "0"); lag.Typical != 0 {
		t.Fatalf("the newest member trails nobody: %.1f", lag.Typical)
	}
	for _, r := range results {
		if r.Label == "repl.member_lag" && r.Instance == "2" {
			t.Fatalf("an arbiter's empty optime was read as lag: %+v", r)
		}
	}
}

// A secondary whose apply rate falls away has an episode below its usual level.
func TestApplyRateDropIsADownwardEpisode(t *testing.T) {
	rate := func(i int) float64 {
		if m := i / 60; m >= 40 && m < 50 {
			return 0
		}
		return 400
	}
	results := run(samplesFor(2*3600, map[string]func(int) float64{
		"serverStatus/metrics/repl/apply/ops": counter(rate),
	}), "n")
	r := find(t, results, "repl.apply_rate", "")
	if r.Shape != "episodic" || len(r.Episodes) != 1 || !r.Episodes[0].Start.Equal(start.Add(40*time.Minute)) || r.Episodes[0].Mean > 50 {
		t.Fatalf("shape %s episodes %+v, want one drop at 10:40 to about nothing", r.Shape, r.Episodes)
	}
	if r.Peak > 50 {
		t.Fatalf("the worst minute of a falling rate is its lowest: %.1f", r.Peak)
	}
}

// The version FTDC recorded picks the definition: a 7.x node is read from concurrentTransactions,
// an 8.0 node from queues.execution, and neither is tried against the other's paths.
func TestTheNodeVersionPicksTheDefinition(t *testing.T) {
	old := "serverStatus/wiredTiger/concurrentTransactions/read/"
	samples := samplesFor(1800, map[string]func(int) float64{
		old + "out":          func(int) float64 { return 128 },
		old + "available":    func(int) float64 { return 0 },
		old + "totalTickets": func(int) float64 { return 128 },
	})
	results, _ := Run(samples, "n", "7.0.14-8", time.Time{}, time.Time{})
	if r := find(t, results, "tickets.read.used", ""); r.Rule().Until != "8.0" || r.Level["near_full"] != 1 {
		t.Fatalf("a 7.0 node read %+v with %+v", r.Level, r.Rule())
	}
	if r := find(t, results, "tickets.read.empty", ""); r.Typical != 60 || r.Level["constant"] != 1 {
		t.Fatalf("a pool empty all along is constant: %+v", r)
	}
	// The same samples claimed by an 8.0 node have no queues.execution: the label is missing, and
	// says which path it looked for rather than falling back to the old one.
	_, skipped := Run(samples, "n", "8.0.4", time.Time{}, time.Time{})
	reason := ""
	for _, s := range skipped {
		if s.Label == "tickets.read.used" {
			reason = s.Reason
		}
	}
	if !strings.Contains(reason, "serverStatus/queues/execution/read/out") {
		t.Fatalf("an 8.0 node without the 8.0 path: reason %q", reason)
	}
}

func TestALabelOutsideItsVersionsSaysWhy(t *testing.T) {
	_, skipped := Run(samplesFor(600, map[string]func(int) float64{}), "n", "6.0.12", time.Time{}, time.Time{})
	for _, s := range skipped {
		if s.Label == "tickets.read.wait" {
			if !strings.Contains(s.Reason, "defined for 7.0 up to 8.0, 8.0 and later; this node runs 6.0.12") {
				t.Fatalf("reason %q", s.Reason)
			}
			return
		}
	}
	t.Fatal("tickets.read.wait was not reported as unavailable on 6.0")
}
