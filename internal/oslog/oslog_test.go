package oslog

import (
	"strings"
	"testing"
	"time"
)

func scan(t *testing.T, text string) *File {
	t.Helper()
	f, err := Scan(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTheKernelKillingMongodIsAnEvent(t *testing.T) {
	f := scan(t, `Sep  8 06:23:24 db1 kernel: oom-kill:constraint=CONSTRAINT_NONE,task=mongod,pid=3185,uid=191
Sep  8 06:23:24 db1 kernel: Out of memory: Killed process 3185 (mongod) total-vm:236957672kB, anon-rss:124916356kB, file-rss:0kB
Sep  8 06:23:32 db1 systemd[1]: mongod.service: Main process exited, code=killed, status=9/KILL
Sep  8 06:23:32 db1 systemd[1]: mongod.service: Failed with result 'signal'.
`)
	if len(f.Events) != 2 {
		t.Fatalf("events %+v, want the kill and the exit", f.Events)
	}
	kill, exit := f.Events[0], f.Events[1]
	if kill.Kind != OOMKill || kill.Process != "mongod" || kill.Detail != "pid 3185, 119.1 GiB resident" || kill.Host != "db1" {
		t.Errorf("kill %+v", kill)
	}
	if exit.Kind != ServiceExit || exit.Detail != "code=killed, status=9/KILL" {
		t.Errorf("exit %+v", exit)
	}
	if want := time.Date(0, 9, 8, 6, 23, 24, 0, time.UTC); !kill.Local.Equal(want) || kill.Year {
		t.Errorf("local %v, want %v with no year", kill.Local, want)
	}
}

// A user's session is not the database: "Started Session 12 of user mongod" says nothing about it.
func TestOnlyTheDatabaseUnitStartsCount(t *testing.T) {
	f := scan(t, `Sep  8 04:00:01 db1 systemd[1]: Started Session 5803 of user mongod.
Sep  8 06:36:47 db1 systemd[1]: Started High-performance, schema-free document-oriented database.
Oct 09 09:20:05 db2 systemd[1]: Started mongod.service - High-performance, schema-free document-oriented database.
`)
	if len(f.Events) != 2 || f.Events[0].Kind != ServiceStart || f.Events[1].Kind != ServiceStart {
		t.Fatalf("events %+v, want the two unit starts only", f.Events)
	}
}

func TestAZonedSyslogLineNeedsNothingElse(t *testing.T) {
	f := scan(t, "\n\n2026-10-12T14:32:47.307964+03:00 svl00pmdb05 kernel: VFS: file-max limit 98000 reached\n")
	if len(f.Events) != 1 {
		t.Fatalf("events %+v", f.Events)
	}
	e := f.Events[0]
	if e.Kind != FileMax || e.Detail != "limit 98000" || !e.At.Equal(time.Date(2026, 10, 12, 11, 32, 47, 307964000, time.UTC)) {
		t.Errorf("event %+v", e)
	}
}

func TestDmesgTimesHaveAYearAndDrift(t *testing.T) {
	f := scan(t, "[Tue Sep  8 06:19:48 2026] Out of memory: Killed process 3185 (mongod) total-vm:1kB, anon-rss:1048576kB\n")
	if len(f.Events) != 1 || !f.Events[0].Year || !f.Events[0].Drifts || f.Events[0].Local.Year() != 2026 {
		t.Fatalf("events %+v", f.Events)
	}
}

// Lines that also write a zoned time say the log's zone; a journal header says its year.
func TestTheZoneIsReadOffLinesThatWroteItTwice(t *testing.T) {
	f := scan(t, `-- Logs begin at Sun 2026-09-06 13:45:20 MST, end at Tue 2026-09-08 23:03:04 MST. --
Sep 08 23:03:04 db1 pbm-agent[3114]: 2026-09-08T23:03:04.000-0700 I [pitr] oplog slicer
Sep 08 06:35:07 db1 mongodb_exporter[3122]: time="2026-09-08T06:35:07-07:00" level=error msg="x"
Apr 01 13:20:53 db1 env[14191]: {"t":{"$date":"2026-09-08T09:20:53.217Z"},"s":"I"}
`)
	offset, year, votes, ok := f.Zone()
	if !ok || offset != -7*time.Hour || year != 2026 || votes != 2 {
		t.Fatalf("zone %v year %d votes %d ok %v; want -7h from the two lines that agree", offset, year, votes, ok)
	}
	if f.Year != 2026 {
		t.Errorf("header year %d", f.Year)
	}
}

func TestOtherProgramsCrashesAreNotEvents(t *testing.T) {
	f := scan(t, `Aug 17 02:30:52 db1 kernel: flow_destroy[2528]: segfault at 0 ip 0 sp 0 error 4
Aug 17 02:30:52 db1 kernel: mongod[2529]: segfault at 0 ip 0 sp 0 error 4
`)
	if len(f.Events) != 1 || f.Events[0].Process != "mongod" {
		t.Fatalf("events %+v, want mongod's segfault only", f.Events)
	}
}
