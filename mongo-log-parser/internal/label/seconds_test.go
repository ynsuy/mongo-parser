package label

import (
	"math"
	"testing"
	"time"
)

// Where a minute mean is wrong: the read ticket pool empties for ten seconds in each of
// ten minutes and is mostly free otherwise. Read on the minute mean the pool is a quarter used and
// nothing ran out; counted in seconds it was empty for a hundred of them.
func TestTenSecondsWithoutTicketsAreSeenInSeconds(t *testing.T) {
	old := "serverStatus/wiredTiger/concurrentTransactions/read/"
	empty := func(i int) bool { m := i / 60; return m >= 60 && m < 70 && i%60 < 10 }
	results := run(samplesFor(3*3600, map[string]func(int) float64{
		old + "out": func(i int) float64 {
			if empty(i) {
				return 128
			}
			return 16
		},
		old + "available": func(i int) float64 {
			if empty(i) {
				return 0
			}
			return 112
		},
		old + "totalTickets": func(int) float64 { return 128 },
	}), "n")

	used := find(t, results, "tickets.read.used", "")
	if used.Peak > 30 || used.PeakLevel["near_full"] > 0 {
		t.Fatalf("the minute mean of use is about a quarter, peak %.1f %v", used.Peak, used.PeakLevel)
	}
	for _, name := range []string{"tickets.read.empty", "tickets.read.scarce"} {
		r := find(t, results, name, "")
		if r.Unit != TimeUnit || r.Peak != 10 || r.PastSeconds != 100 {
			t.Fatalf("%s: peak %.1f %s, %v seconds in all; want 10 s/min and 100 s", name, r.Peak, r.Unit, r.PastSeconds)
		}
		if r.PeakLevel["brief"] != 1 || r.Shape != "episodic" || len(r.Episodes) != 1 || !r.Episodes[0].Start.Equal(start.Add(time.Hour)) {
			t.Fatalf("%s: level %v shape %s episodes %+v, want one brief episode from 11:00", name, r.PeakLevel, r.Shape, r.Episodes)
		}
		if r.Duty == nil || r.Duty.Label != "intermittently" {
			t.Fatalf("%s: ten minutes in three hours is intermittent: %+v", name, r.Duty)
		}
	}
	empty0 := find(t, results, "tickets.read.empty", "")
	if empty0.PeakSecond != 0 || !empty0.PeakSecondAt.Equal(start.Add(time.Hour)) {
		t.Fatalf("the lowest second is no tickets at 11:00:00, got %v at %v", empty0.PeakSecond, empty0.PeakSecondAt)
	}
}

// A pool the server resized: eight tickets for the first hour, sixty-four under load for the
// second, eight again for the third. The pool label shows the change, and scarcity is measured
// against the pool as it was — six of eight free is not scarce, six of sixty-four is.
func TestADynamicPoolIsReadAgainstItsOwnSize(t *testing.T) {
	q := "serverStatus/queues/execution/write/"
	loaded := func(i int) bool { return i >= 3600 && i < 7200 }
	results, _ := Run(samplesFor(3*3600, map[string]func(int) float64{
		q + "totalTickets": func(i int) float64 {
			if loaded(i) {
				return 64
			}
			return 8
		},
		q + "available": func(int) float64 { return 6 },
		q + "out": func(i int) float64 {
			if loaded(i) {
				return 58
			}
			return 2
		},
	}), "n", "8.0.4", time.Time{}, time.Time{})

	pool := find(t, results, "tickets.write.pool", "")
	if pool.Shape != "episodic" || len(pool.Episodes) != 1 || !pool.Episodes[0].Start.Equal(start.Add(time.Hour)) || pool.Episodes[0].Mean != 64 {
		t.Fatalf("the pool grew to 64 for an hour from 11:00: shape %s episodes %+v", pool.Shape, pool.Episodes)
	}
	scarce := find(t, results, "tickets.write.scarce", "")
	if scarce.PastSeconds != 3600 {
		t.Fatalf("six of sixty-four free is scarce for the hour, six of eight never: %v seconds", scarce.PastSeconds)
	}
	used := find(t, results, "tickets.write.used", "")
	if math.Abs(used.Peak-58.0/64*100) > 0.1 || used.PeakLevel["high"] == 0 {
		t.Fatalf("58 of 64 is about 91%% used: %.1f %v", used.Peak, used.PeakLevel)
	}
}

// Twenty seconds pinned each minute and idle the rest averages to about a third busy. cpu.busy says
// a third; cpu.saturated says twenty seconds of every minute.
func TestSecondsPinnedAreCountedNotAveraged(t *testing.T) {
	results := run(samplesFor(2*3600, cpuBySecond(func(i int) float64 {
		if i%60 < 20 {
			return 100
		}
		return 5
	})), "n")
	busy := find(t, results, "cpu.busy", "")
	if math.Abs(busy.Typical-36.7) > 1 || busy.Level["low"] == 0 {
		t.Fatalf("busy %.1f%% %v, want about 37 and low-to-moderate", busy.Typical, busy.Level)
	}
	if busy.PeakSecond != 100 {
		t.Fatalf("the busiest second was 100%%, got %v", busy.PeakSecond)
	}
	saturated := find(t, results, "cpu.saturated", "")
	if saturated.Typical != 20 || saturated.Level["frequent"] != 1 || saturated.Duty.Label != "always" {
		t.Fatalf("saturated %.1f s/min %v duty %+v, want 20, frequent, always", saturated.Typical, saturated.Level, saturated.Duty)
	}
}

// A minute that lost most of its samples is not a minute: twenty seconds of a gap's edge would
// otherwise count as a third of a minute pinned.
func TestAPartialMinuteIsLeftOut(t *testing.T) {
	r := rule(t, "cpu.saturated")
	seconds := make([]Second, 0, 80)
	for i := range 80 {
		seconds = append(seconds, Second{At: start.Add(time.Duration(i) * time.Second), Value: 100, num: 100, den: 1})
	}
	if minutes := Minutes(r, seconds); len(minutes) != 1 || minutes[0].Past != 60 {
		t.Fatalf("a full minute and twenty seconds of the next: %+v", minutes)
	}
}

// A ratio's minute is the ratio of the minute's sums: one slow request in an idle second must not
// weigh as much as a thousand fast ones in a busy second.
func TestARatioMinuteIsTheRatioOfItsSums(t *testing.T) {
	r := rule(t, "disk.await")
	seconds := make([]Second, 0, 60)
	for i := range 60 {
		num, den := 1000.0, 1000.0 // a thousand requests at a millisecond each
		if i == 0 {
			num, den = 500, 1 // one request that took half a second
		}
		seconds = append(seconds, Second{At: start.Add(time.Duration(i) * time.Second), num: num, den: den, Value: r.Signal.finish(num, den)})
	}
	minutes := Minutes(r, seconds)
	if len(minutes) != 1 || math.Abs(minutes[0].Mean-1.008) > 0.001 || minutes[0].Max != 500 {
		t.Fatalf("mean of the minute %+v, want about 1.008 ms with a 500 ms second", minutes)
	}
}

// Opened up, the empty pool is ten stretches of ten seconds, each starting on the minute, and the
// pool size beside it — a paired label — is read over exactly those seconds.
func TestStretchesAreTheSecondsPastTheMark(t *testing.T) {
	old := "serverStatus/wiredTiger/concurrentTransactions/read/"
	empty := func(i int) bool { m := i / 60; return m >= 60 && m < 70 && i%60 < 10 }
	samples := samplesFor(3*3600, map[string]func(int) float64{
		old + "available": func(i int) float64 {
			if empty(i) {
				return 0
			}
			return 112
		},
		old + "totalTickets": func(int) float64 { return 128 },
	})
	r := rule(t, "tickets.read.empty")
	stretches := Stretches(r, Seconds(r, samples, "", time.Time{}, time.Time{}), 5*time.Second)
	if len(stretches) != 10 {
		t.Fatalf("%d stretches, want ten", len(stretches))
	}
	for _, s := range stretches {
		if s.Seconds != 10 || s.Length() != 10*time.Second || s.From.Second() != 0 || s.Worst != 0 {
			t.Fatalf("stretch %+v, want ten seconds from the top of a minute at zero", s)
		}
	}
	pool := rule(t, "tickets.read.pool")
	first := stretches[0]
	w := Summarize(pool, Seconds(pool, samples, "", time.Time{}, time.Time{}), first.From, first.To)
	if w.Samples != 10 || w.Mean != 128 {
		t.Fatalf("the pool over the stretch: %+v", w)
	}
	// A gap of up to the joining gap bridges two runs into one.
	joined := Stretches(r, Seconds(r, samples, "", time.Time{}, time.Time{}), time.Minute)
	if len(joined) != 1 || joined[0].Seconds != 100 {
		t.Fatalf("runs fifty seconds apart joined by a minute's gap: %+v", joined)
	}
}
