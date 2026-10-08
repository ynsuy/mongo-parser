package funnel

import (
	"testing"
	"time"

	"mongo-log-parser/internal/ftdc"
	"mongo-log-parser/internal/label"
)

var start = time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)

// node is three hours of a node whose read ticket pool empties for ten seconds in each of ten
// minutes from 11:00, and whose write pool is half used throughout.
func node() *ftdc.Samples {
	seconds := 3 * 3600
	s := &ftdc.Samples{Values: map[string][]float64{}, Kinds: map[string]string{}}
	for i := range seconds {
		s.Timestamps = append(s.Timestamps, start.Add(time.Duration(i)*time.Second).UnixMilli())
	}
	empty := func(i int) bool { m := i / 60; return m >= 60 && m < 70 && i%60 < 10 }
	set := func(path string, f func(int) float64) {
		values := make([]float64, seconds)
		for i := range values {
			values[i] = f(i)
		}
		s.Values[path] = values
	}
	read := "serverStatus/wiredTiger/concurrentTransactions/read/"
	write := "serverStatus/wiredTiger/concurrentTransactions/write/"
	set(read+"totalTickets", func(int) float64 { return 128 })
	set(read+"available", func(i int) float64 {
		if empty(i) {
			return 0
		}
		return 112
	})
	set(read+"out", func(i int) float64 {
		if empty(i) {
			return 128
		}
		return 16
	})
	set(write+"totalTickets", func(int) float64 { return 128 })
	set(write+"available", func(int) float64 { return 64 })
	set(write+"out", func(int) float64 { return 64 })
	return s
}

func run(t *testing.T) []label.Result {
	t.Helper()
	results, _ := label.Run(node(), "rs0-0", "6.0.12", time.Time{}, time.Time{})
	if len(results) == 0 {
		t.Fatal("no labels")
	}
	label.Relate(results)
	return results
}
