package label

import (
	"testing"
)

// Two labels that rose together are related, the one that rose first leads, and two labels read
// from the same counter are never offered as each other's explanation.
func TestRelatedLabelsAndWhichMovedFirst(t *testing.T) {
	d := "systemMetrics/disks/sda/"
	cpu := "systemMetrics/cpu/"
	slow := func(i int) bool { return i/60 >= 60 && i/60 < 75 }
	waits := func(i int) bool { return i/60 >= 57 && i/60 < 72 } // iowait rises three minutes earlier
	paths := map[string]func(int) float64{
		d + "reads":  counter(func(int) float64 { return 100 }),
		d + "writes": counter(func(int) float64 { return 100 }),
		d + "read_time_ms": counter(func(i int) float64 {
			if slow(i) {
				return 100 * 40
			}
			return 100 * 2
		}),
		d + "write_time_ms": counter(func(int) float64 { return 100 * 2 }),
		d + "io_time_ms":    counter(func(int) float64 { return 300 }),
		cpu + "iowait_ms": counter(func(i int) float64 {
			if waits(i) {
				return 300
			}
			return 10
		}),
		cpu + "idle_ms": counter(func(i int) float64 {
			if waits(i) {
				return 600
			}
			return 890
		}),
		cpu + "user_ms": counter(func(int) float64 { return 100 }),
	}
	for _, other := range []string{"nice_ms", "system_ms", "irq_ms", "softirq_ms", "steal_ms"} {
		paths[cpu+other] = func(int) float64 { return 0 }
	}
	results := run(samplesFor(3*3600, paths), "n")
	Relate(results)
	await := find(t, results, "disk.await", "sda")
	var iowait *Related
	for i, rel := range await.Related {
		if rel.Label == "disk.read_await" {
			t.Fatalf("disk.read_await shares a counter with disk.await and was related to it: %+v", await.Related)
		}
		if rel.Label == "cpu.iowait" {
			iowait = &await.Related[i]
		}
	}
	if iowait == nil || iowait.R < 0.8 || iowait.Lead != 3 {
		t.Fatalf("cpu.iowait should be related, leading by three minutes: %+v", await.Related)
	}
}

// One busy disk among idle ones is concentrated; disks that all idle are not compared.
func TestOneHotDiskIsNotAlike(t *testing.T) {
	paths := map[string]func(int) float64{}
	for dev, busy := range map[string]float64{"sda": 900, "sdb": 50, "sdc": 40, "sdd": 45} {
		paths["systemMetrics/disks/"+dev+"/io_time_ms"] = counter(func(int) float64 { return busy })
	}
	found := Imbalances(run(samplesFor(1800, paths), "n"))
	for _, im := range found {
		if im.Label == "disk.busy" {
			if im.Hottest != "sda" || im.Verdict != "concentrated" || im.Source != "n" || im.Count != 4 {
				t.Fatalf("%+v", im)
			}
			return
		}
	}
	t.Fatalf("no disk.busy imbalance among %+v", found)
}
