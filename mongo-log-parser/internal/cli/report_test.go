package cli

import (
	"encoding/json"
	"mongo-log-parser/internal/analyze"
	"strings"
	"testing"
	"time"
)

// A bundle of FTDC alone used to be answered with "nothing to report on", which hid every label
// in it — a ticket pool that emptied needs no log line to see.
// The sections that survive such a bundle are named here, so a renamed section is caught rather
// than silently dropping that half of the report again.
func TestMetricOnlySectionsAreRealSections(t *testing.T) {
	known := map[string]bool{}
	for _, name := range reportSections {
		known[name] = true
	}
	for name := range metricOnlySections {
		if !known[name] {
			t.Errorf("%q is not a report section, so an FTDC-only bundle silently loses it", name)
		}
	}
	for _, name := range []string{"deployment", "labels"} {
		if !metricOnlySections[name] {
			t.Errorf("%q is drawn from FTDC but an FTDC-only bundle would not print it", name)
		}
	}
	// These need a log line and cannot be filled from metrics.
	for _, name := range []string{"slow", "replication", "ddl", "logs", "signals"} {
		if metricOnlySections[name] {
			t.Errorf("%q needs a log, so an FTDC-only bundle would print it empty", name)
		}
	}
}

// The report sums a shape's minutes up instead of listing them: a week-long shape's every minute
// made the report megabytes. What stays still tells a drip from a burst, and next narrows slow to
// the shape, where every minute is.
func TestReportShapesSumUpTheirMinutes(t *testing.T) {
	at := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	hashed := analyze.Shape{QueryHash: "0A1B2C3D", Timeline: analyze.Timeline{}}
	hashed.Timeline.Add(at, 100, 0)
	hashed.Timeline.Add(at.Add(time.Minute), 400, 0)
	hashed.Timeline.Add(at.Add(time.Minute+time.Second), 900, 0)
	plain := analyze.Shape{Timeline: analyze.Timeline{}}
	plain.Namespace, plain.Operation, plain.Plan = "app.orders", "update", "IDHACK"

	got := reportShapes([]analyze.Shape{hashed, plain}, "/bundle")
	if got[0].ActiveMinutes != 2 || got[0].BusiestMinute == nil || !got[0].BusiestMinute.At.Equal(at.Add(time.Minute)) ||
		got[0].BusiestMinute.Count != 2 || got[0].BusiestMinute.Millis != 1300 || got[0].BusiestMinute.MaxMs != 900 {
		t.Errorf("hashed shape summed up as %+v, %+v", got[0].ActiveMinutes, got[0].BusiestMinute)
	}
	if got[0].Next != "mongo-log-parser slow /bundle --query-hash 0A1B2C3D" {
		t.Errorf("next %q", got[0].Next)
	}
	if got[1].Next != "mongo-log-parser slow /bundle --ns app.orders --op update --plan IDHACK" || got[1].BusiestMinute != nil {
		t.Errorf("unhashed shape: %+v", got[1])
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"timeline"`) {
		t.Error("the report still lists every minute")
	}
}
