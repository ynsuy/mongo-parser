package label

import (
	"testing"
	"time"

	"github.com/ynsuy/mongo-parser/internal/ftdc"
)

var start = time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)

// samplesFor is one-second samples whose values come from one function per path.
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

// counter accumulates a per-second rate into the running total FTDC stores. Samples are asked for
// in order, so the total is carried forward rather than summed again for every second.
func counter(rate func(i int) float64) func(i int) float64 {
	totals := []float64{0}
	return func(i int) float64 {
		for j := len(totals); j <= i; j++ {
			totals = append(totals, totals[j-1]+rate(j))
		}
		return totals[i]
	}
}

// cpuBySecond is a machine whose CPU is busy the share given by busy(second), in percent.
func cpuBySecond(busy func(second int) float64) map[string]func(int) float64 {
	cpu := "systemMetrics/cpu/"
	paths := map[string]func(int) float64{
		cpu + "user_ms": counter(func(i int) float64 { return 10 * busy(i) }),
		cpu + "idle_ms": counter(func(i int) float64 { return 1000 - 10*busy(i) }),
	}
	for _, other := range []string{"nice_ms", "system_ms", "iowait_ms", "irq_ms", "softirq_ms", "steal_ms"} {
		paths[cpu+other] = func(int) float64 { return 0 }
	}
	return paths
}

// cpuAt is a machine whose CPU is busy the share given by busy(minute), in percent.
func cpuAt(busy func(minute int) float64) map[string]func(int) float64 {
	return cpuBySecond(func(i int) float64 { return busy(i / 60) })
}

func find(t *testing.T, results []Result, name, instance string) Result {
	t.Helper()
	for _, r := range results {
		if r.Label == name && r.Instance == instance {
			return r
		}
	}
	t.Fatalf("no %s %s among %d results", name, instance, len(results))
	return Result{}
}

// run is Run for a node whose version FTDC did not record, which is what most of these tests mean.
func run(samples *ftdc.Samples, source string) []Result {
	results, _ := Run(samples, source, "", time.Time{}, time.Time{})
	return results
}

func rule(t *testing.T, name string) Rule {
	t.Helper()
	found := Definitions(name)
	if len(found) == 0 {
		t.Fatalf("no rule %s", name)
	}
	return found[0]
}
