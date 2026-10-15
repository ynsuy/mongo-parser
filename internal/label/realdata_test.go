package label

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ynsuy/mongo-parser/internal/ftdc"
)

// The synthetic tests check what a rule does with a path; only a real capture can check the path is
// one the server writes. A misspelt path is not an error anywhere else — the label is simply never
// read, and its absence is reported as the server not having the metric. Point DBA_FTDC_DIR at a
// diagnostic.data directory to run this.
//
// Machine metrics (systemMetrics) are only written on Linux, and only Linux builds allocate with
// tcmalloc, so a capture from anywhere else checks the rest of the server's paths and not those.
func TestRulesReadPathsTheServerWrites(t *testing.T) {
	dir := os.Getenv("DBA_FTDC_DIR")
	if dir == "" {
		t.Skip("set DBA_FTDC_DIR to a diagnostic.data directory")
	}
	stream, err := ftdc.OpenStream(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, last := stream.Span()
	samples, err := stream.Read(last.Add(-30*time.Minute), last, Wants, nil)
	if err != nil {
		t.Fatal(err)
	}
	version := stream.Identity().Version
	linux := false
	for p := range samples.Values {
		if strings.HasPrefix(p, "systemMetrics/cpu/") {
			linux = true
		}
	}
	_, skipped := Run(samples, "node", version, time.Time{}, time.Time{})
	for _, s := range skipped {
		if !strings.HasPrefix(s.Reason, "not in this node's FTDC: ") {
			continue
		}
		missing := strings.TrimPrefix(s.Reason, "not in this node's FTDC: ")
		switch {
		case (strings.HasPrefix(missing, "systemMetrics/") || strings.HasPrefix(missing, "serverStatus/tcmalloc/")) && !linux:
		case strings.HasPrefix(missing, "replSetGetStatus/"): // a standalone has no replica set
		case strings.Contains(missing, "{i}"): // no disks or members to read
		default:
			t.Errorf("%s on %s reads %s, which this capture does not carry", s.Label, version, missing)
		}
	}
}
