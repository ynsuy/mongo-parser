package analyze

import (
	"encoding/json"
	"testing"
	"time"
)

// Time bounds must travel with observations so a model can distinguish separate
// coverage windows rather than correlate counts that came from different periods.
func TestObservedLogBoundsSurviveJSON(t *testing.T) {
	first := time.Date(2026, 10, 6, 9, 0, 0, 123000000, time.UTC)
	last := first.Add(45 * time.Second)
	groups, health, queries, activity := NewGrouper(), NewHealth(), NewSlowQueries(), NewActivity()
	for _, when := range []time.Time{first, last} {
		for _, r := range []Record{
			at(when, "Slow query", `{"type":"command","ns":"test.items","command":{"find":"items"},"durationMillis":123}`),
			at(when, "Authentication failed", `{}`),
		} {
			r.Source = "node-a"
			for _, sink := range []Sink{groups, health, queries, activity} {
				sink.Add(r)
			}
		}
	}
	for name, value := range map[string]any{
		"group": groups.Groups()[0], "signal": health.Signals()[0],
		"slow shape": queries.Shapes()[0], "activity": activity,
		"migration":        Migration{First: first, Last: last},
		"catalogue change": Change{First: first, Last: last},
		"process":          Process{FirstAt: first, LastAt: last},
	} {
		t.Run(name, func(t *testing.T) {
			assertJSONTimes(t, value, map[string]time.Time{"first_at": first, "last_at": last})
		})
	}
	assertJSONTimes(t, StateSpan{From: first, To: last}, map[string]time.Time{"from": first, "to": last})
	assertJSONTimes(t, Rebuild{At: last, Started: first}, map[string]time.Time{"at": last, "started_at": first})
	assertJSONTimes(t, Burst{At: first}, map[string]time.Time{"at": first})
	assertJSONTimes(t, SetSummary{GapStarted: first, LastPrimaryAt: last, StoppedFrom: first},
		map[string]time.Time{"gap_started_at": first, "last_primary_at": last, "stopped_from": first})
}

func assertJSONTimes(t *testing.T, value any, want map[string]time.Time) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for key, at := range want {
		var got string
		if err := json.Unmarshal(fields[key], &got); err != nil || got != at.Format(time.RFC3339Nano) {
			t.Errorf("%T %s = %s, want %s (err %v)", value, key, fields[key], at.Format(time.RFC3339Nano), err)
		}
	}
}

func TestUnknownLogBoundsAreOmittedFromJSON(t *testing.T) {
	data, err := json.Marshal(NewActivity())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"first_at", "last_at"} {
		if _, exists := fields[key]; exists {
			t.Errorf("unknown %s was reported as a timestamp: %s", key, data)
		}
	}
}
