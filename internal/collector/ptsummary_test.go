package collector

import (
	"strings"
	"testing"
)

// A pt-summary as the collectors write one: sections headed with hashes, a key-value block, and a
// filesystem table whose columns are readable only by position.
const summary = `# Percona Toolkit System Summary Report ######################
        Date | 2026-01-01 00:00:00 UTC (local TZ: GMT +0000)
    Hostname | db-a.example.internal
      Uptime | 3 days,  1 user,  load average: 0.00, 0.00, 0.00
# Processor ##################################################
  Processors | physical = 1, cores = 4, virtual = 8
# Mounted Filesystems ########################################
  Filesystem                      Size Used Type     Opts                              Mountpoint
  /dev/mapper/vg0-data             2.0T  40% xfs      rw,noatime                        /data
  /dev/mapper/vg0-logs              50G  10% xfs      rw,noatime                        /logs
  /dev/sda1                         30G  20% xfs      rw,noatime                        /
  tmpfs                             16G   0% tmpfs    rw,nosuid,nodev                   /dev/shm
# Memory #####################################################
         Total | 32.0G
`

// TestSummaryReadsTheFilesystems is the whole reason this package exists: on a bundle older than
// 8.0 with no initial sync in it, this table is the only thing that knows how much data there is.
func TestSummaryReadsTheFilesystems(t *testing.T) {
	read, err := Parse("PTDEST/pt-summary.out", summary)
	if err != nil {
		t.Fatal(err)
	}
	// The host the report names, not the directory somebody put it in.
	if read.Host != "db-a.example.internal" {
		t.Errorf("host = %q", read.Host)
	}
	if len(read.Filesystems) != 4 {
		t.Fatalf("read %d filesystems, want 4: %+v", len(read.Filesystems), read.Filesystems)
	}
	data := read.Filesystems[0]
	if data.Mount != "/data" || data.Type != "xfs" {
		t.Errorf("first filesystem = %+v", data)
	}
	// 2.0T is 2 * 1024^4, the way df -h writes it, and 40% of that is what is on it.
	if want := 2.0 * (1 << 40); data.Capacity != want {
		t.Errorf("capacity = %v, want %v", data.Capacity, want)
	}
	if want := 0.40 * 2.0 * (1 << 40); data.UsedBytes != want {
		t.Errorf("used = %v, want %v", data.UsedBytes, want)
	}
	// The header row has no size to parse, which is what keeps it out of the table.
	for _, filesystem := range read.Filesystems {
		if filesystem.Device == "Filesystem" {
			t.Error("the header row was read as a filesystem")
		}
	}
	// Every figure has to be traceable to the artifact it came out of.
	if data.Source != "PTDEST/pt-summary.out" {
		t.Errorf("source = %q", data.Source)
	}
}

// TestSummaryRefusesAnythingElse. The store keeps every collector output under one kind, so a text
// that never named itself has to be refused rather than read for a table that looks familiar.
func TestSummaryRefusesAnythingElse(t *testing.T) {
	_, err := Parse("pt-summary.out", "# Mounted Filesystems ####\n  /dev/sda 1.5T 57% xfs rw /data\n")
	if err == nil {
		t.Error("a text that never named itself was read as a pt-summary")
	}
}

// TestSummariesReadsEveryHost: a bundle carries one of these per node, and the figures are per
// machine.
func TestSummariesReadsEveryHost(t *testing.T) {
	texts := map[string]string{"notes.txt": "nothing to do with Percona Toolkit"}
	for _, host := range []string{"db-a", "db-b", "db-c"} {
		texts[host+"/pt-summary.out"] = strings.ReplaceAll(summary, "db-a.example.internal", host+".example.internal")
	}
	found := Summaries(texts)
	if len(found) != 3 {
		t.Fatalf("read %d summaries, want one per host", len(found))
	}
	seen := map[string]bool{}
	for _, read := range found {
		seen[read.Host] = true
		if read.Source == "" {
			t.Error("a summary cannot be traced back to its artifact")
		}
	}
	for _, host := range []string{"db-a.example.internal", "db-b.example.internal", "db-c.example.internal"} {
		if !seen[host] {
			t.Errorf("no summary for %s", host)
		}
	}
}
