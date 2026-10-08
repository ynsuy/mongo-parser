package cli

import (
	"mongo-log-parser/internal/analyze"
	"strings"
	"testing"
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

// A wait's "Look with" names a report section a reader will type back; a section that no longer
// exists turns the suggestion into an error.
func TestWaitHintsNameRealSections(t *testing.T) {
	known := map[string]bool{}
	for _, name := range reportSections {
		known[name] = true
	}
	queries := analyze.NewSlowQueries()
	_, waits := queries.WaitTotals()
	waits = append(waits, analyze.UnaccountedWait())
	for _, w := range waits {
		only, found := strings.CutPrefix(w.Next, "report --only ")
		if !found {
			continue
		}
		for _, name := range strings.Split(only, ",") {
			if !known[name] {
				t.Errorf("wait %s suggests report section %q, which does not exist", w.Name, name)
			}
		}
	}
}
