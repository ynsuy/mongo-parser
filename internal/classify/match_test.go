package classify

import (
	"testing"
	"time"

	"github.com/ynsuy/mongo-parser/internal/ftdc"
	"github.com/ynsuy/mongo-parser/internal/label"
)

var start = time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)

func samplesFor(seconds int, paths map[string]func(i int) float64) *ftdc.Samples {
	s := &ftdc.Samples{Values: map[string][]float64{}, Kinds: map[string]string{}}
	for i := range seconds {
		s.Timestamps = append(s.Timestamps, start.Add(time.Duration(i)*time.Second).UnixMilli())
	}
	for p, f := range paths {
		values := make([]float64, seconds)
		for i := range values {
			values[i] = f(i)
		}
		s.Values[p] = values
	}
	return s
}

// counter accumulates a per-second rate into the running total FTDC stores, the way a real counter
// would, so the rule's own rate decoder recovers it exactly.
func counter(rate func(i int) float64) func(i int) float64 {
	totals := []float64{0}
	return func(i int) float64 {
		for j := len(totals); j <= i; j++ {
			totals = append(totals, totals[j-1]+rate(j))
		}
		return totals[i]
	}
}

func run(samples *ftdc.Samples, source, version string) []label.Result {
	results, _ := label.Run(samples, source, version, time.Time{}, time.Time{})
	return results
}

func idleCPU(paths map[string]func(int) float64) {
	for _, other := range []string{"nice_ms", "system_ms", "irq_ms", "softirq_ms", "steal_ms"} {
		paths["systemMetrics/cpu/"+other] = func(int) float64 { return 0 }
	}
}

// A disk busy and slow, with CPU time going to I/O wait, is named io_saturation; the three labels
// behind it travel with the match.
func TestIOSaturationMatchesAllThreeLabelsTogether(t *testing.T) {
	d, cpu := "systemMetrics/disks/sda/", "systemMetrics/cpu/"
	paths := map[string]func(int) float64{
		d + "io_time_ms":    counter(func(int) float64 { return 900 }), // 90% busy
		d + "reads":         counter(func(int) float64 { return 100 }), // a hundred reads a second
		d + "writes":        counter(func(int) float64 { return 0 }),
		d + "read_time_ms":  counter(func(int) float64 { return 2000 }), // 20 ms a read
		d + "write_time_ms": counter(func(int) float64 { return 0 }),
		cpu + "user_ms":     counter(func(int) float64 { return 50 }),
		cpu + "idle_ms":     counter(func(int) float64 { return 800 }),
		cpu + "iowait_ms":   counter(func(int) float64 { return 150 }), // 15% iowait
	}
	idleCPU(paths)
	results := run(samplesFor(1800, paths), "n", "")
	matches := Classify(results)
	var found *Match
	for i := range matches {
		if matches[i].Name == "io_saturation" {
			found = &matches[i]
		}
	}
	if found == nil {
		t.Fatalf("io_saturation did not match: %+v", matches)
	}
	if len(found.Because) != 3 {
		t.Fatalf("want three conditions behind it: %+v", found.Because)
	}
}

// Disk busy alone, with CPU never waiting on it and the disk still answering fast, does not say
// io_saturation: that is what a single label read alone would miss.
func TestABusyDiskAloneIsNotIOSaturation(t *testing.T) {
	d, cpu := "systemMetrics/disks/sda/", "systemMetrics/cpu/"
	paths := map[string]func(int) float64{
		d + "io_time_ms":    counter(func(int) float64 { return 900 }),
		d + "reads":         counter(func(int) float64 { return 5000 }),
		d + "writes":        counter(func(int) float64 { return 0 }),
		d + "read_time_ms":  counter(func(int) float64 { return 500 }), // a tenth of a ms each
		d + "write_time_ms": counter(func(int) float64 { return 0 }),
		cpu + "user_ms":     counter(func(int) float64 { return 50 }),
		cpu + "idle_ms":     counter(func(int) float64 { return 940 }),
		cpu + "iowait_ms":   counter(func(int) float64 { return 10 }),
	}
	idleCPU(paths)
	results := run(samplesFor(1800, paths), "n", "")
	for _, m := range Classify(results) {
		if m.Name == "io_saturation" {
			t.Fatalf("a fast, busy disk should not be io_saturation: %+v", m)
		}
	}
}

// Two disks, each only half the pattern — one busy but fast, the other slow but not busy — must not
// be blended into one match: disk.busy and disk.await have to agree on which disk.
func TestIOSaturationDoesNotBlendTwoDisks(t *testing.T) {
	cpu := "systemMetrics/cpu/"
	fastBusy, slowIdle := "systemMetrics/disks/sda/", "systemMetrics/disks/nvme1/"
	paths := map[string]func(int) float64{
		// sda: busy (90%) but answers fast (0.1 ms) — half of io_saturation.
		fastBusy + "io_time_ms":    counter(func(int) float64 { return 900 }),
		fastBusy + "reads":         counter(func(int) float64 { return 5000 }),
		fastBusy + "writes":        counter(func(int) float64 { return 0 }),
		fastBusy + "read_time_ms":  counter(func(int) float64 { return 500 }),
		fastBusy + "write_time_ms": counter(func(int) float64 { return 0 }),
		// nvme1: slow (20 ms) but not busy (20%) — the other half.
		slowIdle + "io_time_ms":    counter(func(int) float64 { return 200 }),
		slowIdle + "reads":         counter(func(int) float64 { return 100 }),
		slowIdle + "writes":        counter(func(int) float64 { return 0 }),
		slowIdle + "read_time_ms":  counter(func(int) float64 { return 2000 }),
		slowIdle + "write_time_ms": counter(func(int) float64 { return 0 }),
		cpu + "user_ms":            counter(func(int) float64 { return 50 }),
		cpu + "idle_ms":            counter(func(int) float64 { return 800 }),
		cpu + "iowait_ms":          counter(func(int) float64 { return 150 }), // elevated, satisfied either way
	}
	idleCPU(paths)
	results := run(samplesFor(1800, paths), "n", "")
	for _, m := range Classify(results) {
		if m.Name == "io_saturation" {
			t.Fatalf("no single disk is both busy and slow; io_saturation should not fire by mixing them: %+v", m)
		}
	}
}

// A CPU busy with threads queued for a core at once is named cpu_saturation.
func TestCPUSaturationMatchesBusyAndQueuedTogether(t *testing.T) {
	cpu := "systemMetrics/cpu/"
	paths := map[string]func(int) float64{
		cpu + "user_ms":                       counter(func(int) float64 { return 840 }), // 84% busy
		cpu + "idle_ms":                       counter(func(int) float64 { return 160 }),
		cpu + "iowait_ms":                     counter(func(int) float64 { return 0 }),
		"systemMetrics/cpu/procs_running":     func(int) float64 { return 4 },
		"systemMetrics/cpu/num_logical_cores": func(int) float64 { return 4 }, // one runnable thread per core
	}
	idleCPU(paths)
	results := run(samplesFor(1800, paths), "n", "8.0.0")
	mustMatch(t, results, "cpu_saturation")
}

// mustMatch fails the test unless name is among the classifiers Classify finds.
func mustMatch(t *testing.T, results []label.Result, name string) {
	t.Helper()
	for _, m := range Classify(results) {
		if m.Name == name {
			return
		}
	}
	t.Fatalf("%s did not match: %+v", name, Classify(results))
}

// Memory tight and the host swapping at the same time is named memory_pressure, proving a gauge
// classifier (memory.used, memory.swap_used) across a different pack than saturation's.
func TestMemoryPressureMatchesUsedAndSwapping(t *testing.T) {
	paths := map[string]func(int) float64{
		"systemMetrics/memory/MemTotal_kb":     func(int) float64 { return 10000 },
		"systemMetrics/memory/MemAvailable_kb": func(int) float64 { return 1200 }, // 88% used
		"systemMetrics/memory/SwapTotal_kb":    func(int) float64 { return 10000 },
		"systemMetrics/memory/SwapFree_kb":     func(int) float64 { return 9000 }, // 10% swapped
	}
	mustMatch(t, run(samplesFor(1800, paths), "n", "6.0.12"), "memory_pressure")
}

// The cache pinned near full with application threads evicting is named cache_pressure.
func TestCachePressureMatchesFullAndEvicting(t *testing.T) {
	paths := map[string]func(int) float64{
		"serverStatus/wiredTiger/cache/bytes currently in the cache":         func(int) float64 { return 990000 }, // 99% full
		"serverStatus/wiredTiger/cache/maximum bytes configured":             func(int) float64 { return 1000000 },
		"serverStatus/wiredTiger/cache/pages evicted by application threads": counter(func(int) float64 { return 5 }),
	}
	mustMatch(t, run(samplesFor(1800, paths), "n", "7.0.0"), "cache_pressure")
}

// The read ticket pool repeatedly empty with operations measurably queued is read_ticket_exhaustion.
func TestReadTicketExhaustionMatchesScarceAndWaiting(t *testing.T) {
	read := "serverStatus/wiredTiger/concurrentTransactions/read/"
	paths := map[string]func(int) float64{
		read + "available":             func(int) float64 { return 5 }, // 5% of the pool left
		read + "totalTickets":          func(int) float64 { return 100 },
		read + "totalTimeQueuedMicros": counter(func(int) float64 { return 50000 }), // 50 ms/s queued
	}
	mustMatch(t, run(samplesFor(1800, paths), "n", "7.3.0"), "read_ticket_exhaustion")
}

// The write ticket pool repeatedly empty with operations measurably queued is write_ticket_exhaustion.
func TestWriteTicketExhaustionMatchesScarceAndWaiting(t *testing.T) {
	write := "serverStatus/wiredTiger/concurrentTransactions/write/"
	paths := map[string]func(int) float64{
		write + "available":             func(int) float64 { return 5 },
		write + "totalTickets":          func(int) float64 { return 100 },
		write + "totalTimeQueuedMicros": counter(func(int) float64 { return 50000 }),
	}
	mustMatch(t, run(samplesFor(1800, paths), "n", "7.3.0"), "write_ticket_exhaustion")
}

// A member steadily behind while flow control is lagged at the same time is replication_lag: two
// replica set members (behind() reads the gap to the newest instance) plus the primary's own flag.
func TestReplicationLagMatchesBehindMemberAndFlowControl(t *testing.T) {
	const base = 1_700_000_000_000 // an arbitrary, realistic epoch millisecond
	paths := map[string]func(int) float64{
		"replSetGetStatus/members/0/lastAppliedWallTime": func(i int) float64 { return base + float64(i)*1000 },
		"replSetGetStatus/members/1/lastAppliedWallTime": func(i int) float64 { return base + float64(i)*1000 - 10000 }, // 10s behind
		"serverStatus/flowControl/isLagged":              func(int) float64 { return 1 },
	}
	mustMatch(t, run(samplesFor(1800, paths), "n", "7.0.0"), "replication_lag")
}

// Connections repeatedly scarce with some actually rejected is connection_storm.
func TestConnectionStormMatchesScarceAndRejected(t *testing.T) {
	paths := map[string]func(int) float64{
		"serverStatus/connections/available": func(int) float64 { return 5 }, // 5% of the limit left
		"serverStatus/connections/current":   func(int) float64 { return 95 },
		"serverStatus/connections/rejected":  counter(func(int) float64 { return 0.5 }),
	}
	mustMatch(t, run(samplesFor(1800, paths), "n", "7.0.0"), "connection_storm")
}
