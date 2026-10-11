package label

import (
	"strings"
	"testing"
)

func TestRulesLoadAndValidate(t *testing.T) {
	if _, err := load(); err != nil {
		t.Fatal(err)
	}
	if len(Rules) < 40 {
		t.Fatalf("%d rules loaded", len(Rules))
	}
}

// Every rule says what it measures, and a time view says it in seconds a minute whatever the
// signal is in, so two labels of one kind always read alike.
func TestEveryTimeViewIsInSecondsAMinute(t *testing.T) {
	for _, r := range Rules {
		if r.Timed() && r.Unit() != TimeUnit {
			t.Errorf("%s is a time view in %q", r.Name, r.Unit())
		}
		if !r.Timed() && r.Unit() != r.Signal.Unit {
			t.Errorf("%s reads its level in %q, not its signal's %q", r.Name, r.Unit(), r.Signal.Unit)
		}
	}
}

func TestBandsOverlapRatherThanFlip(t *testing.T) {
	busy := rule(t, "cpu.busy")
	got := busy.degrees(91.5)
	if got["high"] <= 0 || got["very_high"] <= 0 {
		t.Fatalf("91.5%% should be partly high and partly very_high: %v", got)
	}
	got = busy.degrees(99)
	if got["very_high"] != 1 || got["high"] != 0 {
		t.Fatalf("99%% is very_high and only that: %v", got)
	}
}

// A rule a reviewer gets wrong is refused when the binary starts, not discovered on a bundle.
func TestMalformedRulesAreRefused(t *testing.T) {
	threshold := 1.0
	base := func() Rule {
		return Rule{Name: "x", Title: "x", Doc: "x", View: ViewMean, MinChange: 1,
			Signal: Signal{Mode: "gauge", Unit: "%", Num: []string{"a"}, Scale: 1}}
	}
	cases := map[string]func(*Rule){
		"needs a threshold":   func(r *Rule) { r.View = ViewTimeBelow },
		"only read by a time": func(r *Rule) { r.Threshold = &threshold },
		"none of mean":        func(r *Rule) { r.View = "p99" },
		"needs a unit":        func(r *Rule) { r.Signal.Unit = "" },
		"leave a gap": func(r *Rule) {
			r.Levels = []Level{{Name: "low", Down: []float64{1, 2}}, {Name: "high", Up: []float64{3, 4}}}
		},
		"must be a down ramp": func(r *Rule) { r.Levels = []Level{{Name: "a", Up: []float64{1, 2}}, {Name: "b", Up: []float64{1, 2}}} },
		"must be an up ramp": func(r *Rule) {
			r.Levels = []Level{{Name: "a", Down: []float64{1, 2}}, {Name: "b", Down: []float64{1, 2}}}
		},
		"min_change must be": func(r *Rule) { r.MinChange = 0 },
		"every path names":   func(r *Rule) { r.Signal.Den = []string{"disks/{i}/x"} },
	}
	for want, breakIt := range cases {
		r := base()
		breakIt(&r)
		err := r.validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error containing %q, got %v", want, err)
		}
	}
	if err := base().validate(); err != nil {
		t.Errorf("the base rule is valid: %v", err)
	}
	if err := checkPairs([]Rule{{Name: "a", PairsWith: []string{"b"}}}); err == nil {
		t.Error("a pairing with a label that does not exist was accepted")
	}
}

func TestWantsCoversInstancesAndAlternatives(t *testing.T) {
	for _, p := range []string{
		"systemMetrics/disks/sda/io_queued_ms",
		"serverStatus/queues/execution/read/totalTickets",
		"serverStatus/wiredTiger/concurrentTransactions/write/available",
		"systemMetrics/memory/MemAvailable_kb",
	} {
		if !Wants(p) {
			t.Errorf("%s is read by a rule and not wanted", p)
		}
	}
	if Wants("systemMetrics/disks/sda/extra/io_time_ms") || Wants("serverStatus/asserts/regular") {
		t.Error("a path no rule reads is wanted")
	}
}

func TestVersionRangesMayNotOverlap(t *testing.T) {
	a := Rule{Name: "x", Until: "7.0"}
	b := Rule{Name: "x", Since: "7.0"}
	c := Rule{Name: "x", Since: "6.0"}
	if overlap(a, b) {
		t.Error("until 7.0 and since 7.0 do not overlap")
	}
	if !overlap(a, c) || !overlap(b, c) {
		t.Error("since 6.0 overlaps both")
	}
}
