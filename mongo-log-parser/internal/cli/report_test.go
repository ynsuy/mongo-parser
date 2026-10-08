package cli

import "testing"

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
