package analyze

import (
	"testing"
	"time"

	"mongo-log-parser/internal/store"
)

// The log caught nothing; the FTDC saw two runs and the OS saw the first one killed. Together they
// say the node restarted once, after an OOM kill, on a bigger cache.
func TestFTDCAndTheOSFillInWhatTheLogMissed(t *testing.T) {
	at := func(day, hour int) time.Time { return time.Date(2026, 9, day, hour, 0, 0, 0, time.UTC) }
	runs := []store.Run{
		{Node: "db1", Started: at(1, 0), First: at(5, 0), Last: at(8, 13), Version: "8.0.12", CacheBytes: 4 << 30, MemKB: 32 << 20, CPUs: 8},
		{Node: "db1", Started: at(8, 14), First: at(8, 14), Last: at(9, 0), Version: "8.0.12", CacheBytes: 8 << 30, MemKB: 32 << 20, CPUs: 8},
	}
	events := []store.SystemEvent{
		{Node: "db1", At: at(8, 13), Kind: "oom_kill", Process: "mongod", Detail: "pid 1, 30.0 GiB resident"},
		{Node: "db1", At: at(8, 13).Add(8 * time.Second), Kind: "service_exit", Process: "mongod", Detail: "code=killed, status=9/KILL"},
		{Node: "db1", At: at(8, 14).Add(time.Second), Kind: "service_start", Process: "mongod"},
		{Node: "db1", At: at(7, 0), Kind: "file_max", Detail: "limit 98000"},
	}
	d := NewDeployment()
	d.Join(runs, events, at(5, 0), time.Time{})
	processes := d.Processes()
	if len(processes) != 1 {
		t.Fatalf("processes %+v", processes)
	}
	p := processes[0]
	if p.Starts != 1 || !p.LastStartAt.Equal(at(8, 14)) {
		t.Errorf("starts %d last %v: the systemd start and the FTDC run are one restart", p.Starts, p.LastStartAt)
	}
	if len(p.Endings) != 1 || p.Endings[0].Kind != "oom_kill" || p.Unclean != 1 {
		t.Errorf("endings %+v unclean %d: the kill and systemd's account of it are one ending", p.Endings, p.Unclean)
	}
	if len(p.Changes) != 1 || p.Changes[0].What != "cache" || p.Changes[0].From != "4G" || p.Changes[0].To != "8G" {
		t.Errorf("changes %+v", p.Changes)
	}
	if p.CacheGB != 8 || p.MemoryGB != 32 || p.CPUs != 8 || p.Version != "8.0.12" {
		t.Errorf("latest run read as cache %v memory %v cpus %d version %s", p.CacheGB, p.MemoryGB, p.CPUs, p.Version)
	}
	summary := d.Summarize()
	if summary.Restarts != 1 || summary.OOMKills != 1 {
		t.Errorf("summary %+v", summary)
	}
	if len(d.SystemEvents()) != 3 {
		t.Errorf("system events %d", len(d.SystemEvents()))
	}
}
