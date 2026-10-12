package render

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mongo-log-parser/internal/series"
)

func TestGnuplotDataPreservesZerosGapsAndSelectedAggregation(t *testing.T) {
	row := series.Series{BucketSeconds: 60, PlotAggregation: "avg", Buckets: []series.Bucket{
		{Start: "2026-10-09T06:00:00Z", Samples: 2, Min: 0, Avg: 0, Max: 10},
		{Start: "2026-10-09T06:02:00Z", Samples: 2, Min: 2, Avg: 3, Max: 20},
	}}
	data := gnuplotData(row)
	if !strings.Contains(data, "1791525600.000 0\n") || !strings.Contains(data, "1791525660.000 ?\n") || !strings.Contains(data, "1791525720.000 3\n") {
		t.Fatalf("zero, missing bin or mean lost: %s", data)
	}
	row.Low = true
	if !strings.Contains(gnuplotData(row), "1791525720.000 2\n") {
		t.Fatal("mean hid depletion")
	}
	row.Low, row.PlotAggregation = false, ""
	if !strings.Contains(gnuplotData(row), "1791525720.000 20\n") {
		t.Fatal("peak was lost")
	}
}

func TestGnuplotDumbOutputPreservesEvidenceAndDoesNotExecuteNames(t *testing.T) {
	if _, err := exec.LookPath("gnuplot"); err != nil {
		t.Skip("gnuplot not installed")
	}
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	row := sampleSeries("node-a", "disk.busy", 0, 0, 2, 87, 90)
	row.Label, row.Unit, row.PlotAggregation = "disk.busy sda [%]", "%", "avg"
	row.Source = "node'; system 'touch " + marker + "'; # `touch " + marker + "`"
	rows := []series.Series{row}
	before, _ := json.Marshal(rows)
	output, err := GnuplotReport(rows, GnuplotOptions{Width: 100, Height: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "UTC") || !strings.Contains(output, "sample-weighted mean per bin") || !strings.Contains(output, "disk.busy") {
		t.Fatalf("plot lost labels or aggregation: %s", output)
	}
	for _, r := range output {
		if r == 27 || r > 127 {
			t.Fatalf("expected plain ASCII, found %U", r)
		}
	}
	after, _ := json.Marshal(rows)
	if string(before) != string(after) {
		t.Fatal("plotting changed structured evidence")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a series name executed a shell command")
	}
}

func TestGnuplotMissingDependencyExplainsHowToInstall(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := GnuplotReport([]series.Series{sampleSeries("node-a", "x", 0, 1, 2)}, GnuplotOptions{Width: 80, Height: 8})
	if err == nil || !strings.Contains(err.Error(), "brew install gnuplot") {
		t.Fatalf("missing dependency was hidden: %v", err)
	}
}
