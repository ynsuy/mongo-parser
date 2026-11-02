package funnel

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/ynsuy/mongo-parser/internal/label"
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

// Opening the emptied read pool finds its ten stretches, and reads the three longest closely.
func TestOpeningALabelReadsItsStretches(t *testing.T) {
	samples := node()
	results := run(t)
	opened := Open(target(t, results, "tickets.read.empty"), samples, time.Time{}, time.Time{}, time.Time{}, 0)
	if opened.StretchCount != 10 || len(opened.Stretches) != 10 || len(opened.Focus) != focusKept {
		t.Fatalf("%d stretches, %d listed, %d windows", opened.StretchCount, len(opened.Stretches), len(opened.Focus))
	}
	f := opened.Focus[0]
	if f.Self.Past != 10 || f.Self.Min != 0 {
		t.Fatalf("the window holds the ten empty seconds: %+v", f.Self)
	}
}

// Asked for a moment, the window comes back second by second, with repeated seconds folded.
func TestAMomentComesBackSecondBySecond(t *testing.T) {
	samples := node()
	results := run(t)
	at := start.Add(time.Hour + 5*time.Second)
	opened := Open(target(t, results, "tickets.read.empty"), samples, time.Time{}, time.Time{}, at, 2*time.Minute)
	rows := opened.Focus[0].Seconds
	// 10:59:05 to 11:01:05: free, empty for ten seconds, free, empty again to the window's end.
	if len(rows) != 4 || rows[3].Repeat != 5 {
		t.Fatalf("%d rows, want four runs ending in six empty seconds: %+v", len(rows), rows)
	}
	if rows[1].Value != 0 || rows[1].Repeat != 9 || !rows[1].At.Equal(start.Add(time.Hour)) {
		t.Fatalf("the first empty run starts at 11:00:00 for ten seconds, none left: %+v", rows[1])
	}
}

// A second with no sample is written as null, so a window with a gap still comes back as JSON
// rather than as an encoding error.
func TestASecondWithoutASampleIsNull(t *testing.T) {
	got, err := json.Marshal(Row{At: start, Value: math.NaN()})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"at":"2026-09-03T10:00:00Z","value":null}`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
