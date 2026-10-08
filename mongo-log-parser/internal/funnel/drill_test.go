package funnel

import (
	"math"
	"testing"
	"time"

	"mongo-log-parser/internal/label"
)

func target(t *testing.T, results []label.Result, name string) label.Result {
	t.Helper()
	for _, r := range results {
		if r.Label == name {
			return r
		}
	}
	t.Fatalf("no %s", name)
	return label.Result{}
}

// Opening the emptied read pool finds its ten stretches, reads the three longest closely, and reads
// what it pairs with over exactly those seconds: the pool was full size and fully used then, where
// usually an eighth of it was.
func TestOpeningALabelReadsItsPairsOverItsStretches(t *testing.T) {
	samples := node()
	results := run(t)
	opened := Open(target(t, results, "tickets.read.empty"), results, samples, time.Time{}, time.Time{}, time.Time{}, 0)
	if opened.StretchCount != 10 || len(opened.Stretches) != 10 || len(opened.Focus) != focusKept {
		t.Fatalf("%d stretches, %d listed, %d windows", opened.StretchCount, len(opened.Stretches), len(opened.Focus))
	}
	f := opened.Focus[0]
	if f.Self.Past != 10 || f.Self.Min != 0 {
		t.Fatalf("the window holds the ten empty seconds: %+v", f.Self)
	}
	var used, pool *Pair
	for i := range f.Pairs {
		switch f.Pairs[i].Label {
		case "tickets.read.used":
			used = &f.Pairs[i]
		case "tickets.read.pool":
			pool = &f.Pairs[i]
		}
	}
	if used == nil || used.Window.Max != 100 || math.Abs(used.Usual.Mean-12.9) > 0.5 {
		t.Fatalf("use peaked at 100%% in the window and is usually about 13%%: %+v", used)
	}
	if pool == nil || pool.Window.Mean != 128 {
		t.Fatalf("the pool was 128 throughout: %+v", pool)
	}
	// A pair this node never labelled is named, not dropped.
	missing := false
	for _, p := range f.Pairs {
		if p.Label == "tickets.read.wait" && p.Missing != "" {
			missing = true
		}
	}
	if !missing {
		t.Errorf("tickets.read.wait is not reported before 7.0 and should say so: %+v", f.Pairs)
	}
}

// Asked for a moment, the window comes back second by second, with repeated seconds folded.
func TestAMomentComesBackSecondBySecond(t *testing.T) {
	samples := node()
	results := run(t)
	at := start.Add(time.Hour + 5*time.Second)
	opened := Open(target(t, results, "tickets.read.empty"), results, samples, time.Time{}, time.Time{}, at, 2*time.Minute)
	rows := opened.Focus[0].Seconds
	// 10:59:05 to 11:01:05: free, empty for ten seconds, free, empty again to the window's end.
	if len(rows) != 4 || rows[3].Repeat != 5 {
		t.Fatalf("%d rows, want four runs ending in six empty seconds: %+v", len(rows), rows)
	}
	if rows[1].Values[0] != 0 || rows[1].Values[1] != 100 || rows[1].Repeat != 9 || !rows[1].At.Equal(start.Add(time.Hour)) {
		t.Fatalf("the first empty run starts at 11:00:00 for ten seconds, none left and all in use: %+v", rows[1])
	}
	if len(opened.Columns(opened.Focus[0])) != len(rows[0].Values) {
		t.Fatal("a column name for every value")
	}
}
