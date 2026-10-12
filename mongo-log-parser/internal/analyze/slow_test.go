package analyze

import (
	"strings"
	"testing"
	"time"
)

// slowQuery is one Slow query line on a connection, which is the pair of facts that ties a
// statement to the command that carried it.
func slowQuery(when time.Time, context, attr string) Record {
	record := at(when, "Slow query", attr)
	record.Ctx = context
	return record
}

func TestLoggedShapeIdentity(t *testing.T) {
	s := NewSlowQueries()
	for _, attr := range []string{
		`{"ns":"db.$cmd","queryHash":"ABC123EF","command":{"find":"items"},"planSummary":"COLLSCAN"}`,
		`{"ns":"db.items","queryHash":"ABC123EF","command":{"find":"items"},"planSummary":"COLLSCAN"}`,
		`{"ns":"db.other","queryHash":"ABC123EF","command":{"find":"other"},"planSummary":"COLLSCAN"}`,
		`{"ns":"db.items","queryHash":"ABC123EF","command":{"find":"items"},"planSummary":"IXSCAN"}`,
	} {
		s.Add(rec("Slow query", attr))
	}
	shapes := s.Shapes()
	if len(shapes) != 3 {
		t.Fatalf("wanted 3 namespace/plan groups: %+v", shapes)
	}
	for _, shape := range shapes {
		if shape.ID != "ABC123EF" || shape.QueryHash != "ABC123EF" {
			t.Fatalf("changed logged hash: %+v", shape)
		}
		if shape.Namespace == "db.items" && shape.Plan == "COLLSCAN" && shape.Count != 2 {
			t.Fatal("$cmd did not resolve")
		}
	}
}

func TestMissingHashAndNewHash(t *testing.T) {
	s := NewSlowQueries()
	s.Add(rec("Slow query", `{"ns":"db.items","command":{"find":"items","filter":{"x":42}}}`))
	shape := s.Shapes()[0]
	if shape.ID != "" || shape.GroupedBy != "namespace" || shape.Label() != "not logged" {
		t.Fatalf("invented shape: %+v", shape)
	}
	s = NewSlowQueries()
	s.Add(rec("Slow query", `{"ns":"db.items","queryHash":"OLD12345","planCacheShapeHash":"NEW12345","command":{"find":"items"}}`))
	shape = s.Shapes()[0]
	if shape.ID != "NEW12345" || shape.QueryHash != "OLD12345" || shape.PlanCacheShapeHash != "NEW12345" || shape.GroupedBy != "planCacheShapeHash" {
		t.Fatalf("lost logged hashes: %+v", shape)
	}
}

// waitLines is one update that spent almost all of five seconds waiting: for write concern, for
// flow control, for a lock. On an idle machine this is the whole diagnosis, and a table of
// durations cannot tell it apart from a query that did five seconds of work.
const waitLines = `{"t":{"$date":"2026-09-03T10:00:00Z"},"s":"I","c":"WRITE","id":51803,"ctx":"conn1","msg":"Slow query","attr":{"type":"update","ns":"app.events","command":{"update":"events"},"planSummary":"IDHACK","queryHash":"FF00AA11","durationMillis":5000,"keysExamined":1,"docsExamined":1,"nreturned":0,"cpuNanos":150000000,"locks":{"Global":{"acquireCount":{"w":1},"acquireWaitCount":{"w":1},"timeAcquiringMicros":{"w":200000}}},"flowControl":{"acquireCount":1,"timeAcquiringMicros":800000},"waitForWriteConcernDurationMillis":3600,"storage":{"data":{"bytesRead":4096,"timeReadingMicros":100000}}}}
`

func readSlow(t *testing.T, source, text string) *SlowQueries {
	t.Helper()
	queries := NewSlowQueries()
	feed(t, source, text, queries)
	return queries
}

func TestSlowQueryBreaksDownWhereTheTimeWent(t *testing.T) {
	shapes := readSlow(t, "node-a", waitLines).Shapes()
	if len(shapes) != 1 {
		t.Fatalf("read %d shapes, want 1", len(shapes))
	}
	shape := shapes[0]
	if !shape.Waited() {
		t.Fatal("the shape reports no waits; every figure below is in the fixture")
	}
	// The microsecond fields are reported in milliseconds, so they can be read against the duration.
	if shape.FlowControlWait.Sum != 800 || shape.LockWait.Sum != 200 || shape.StorageWait.Sum != 100 {
		t.Errorf("flow control %v, lock %v, storage %v ms; want 800, 200, 100",
			shape.FlowControlWait.Sum, shape.LockWait.Sum, shape.StorageWait.Sum)
	}
	if shape.WriteConcernWait.Sum != 3600 || shape.CPU.Sum != 150 {
		t.Errorf("write concern %v ms, cpu %v ms; want 3600 and 150", shape.WriteConcernWait.Sum, shape.CPU.Sum)
	}
	if shape.LockAcquireWaits.Sum != 1 {
		t.Errorf("lock acquire waits = %v, want 1", shape.LockAcquireWaits.Sum)
	}

	waits := shape.Waits()
	if len(waits) == 0 || waits[0].Name != "write concern" {
		t.Fatalf("waits = %+v, want write concern first", waits)
	}
	if got := waits[0].Share; got < 0.71 || got > 0.73 {
		t.Errorf("write concern share = %.3f, want about 0.72", got)
	}
	// What the server did not account for is named rather than left out, or the shares would add
	// up to less than the duration with nothing to say why.
	remainder := 5000.0 - (3600 + 800 + 200 + 100)
	for _, wait := range waits {
		if wait.Name == "unaccounted" && wait.Millis != remainder {
			t.Errorf("unaccounted = %v ms, want %v", wait.Millis, remainder)
		}
	}
}

// A router's own log says only that it waited for a shard, which is worth saying plainly rather
// than letting the duration read as work the router did.
func TestSlowQueryReadsARouterWaitingOnShards(t *testing.T) {
	queries := readSlow(t, "router",
		`{"t":{"$date":"2026-09-03T10:00:00Z"},"s":"I","c":"COMMAND","id":51803,"ctx":"conn1","msg":"Slow query","attr":{"type":"command","ns":"app.events","command":{"update":"events"},"remoteOpWaitMillis":4900,"durationMillis":5000}}`+"\n")

	waits := queries.Shapes()[0].Waits()
	if len(waits) == 0 || waits[0].Name != "remote shards" || waits[0].Millis != 4900 {
		t.Fatalf("waits = %+v, want remote shards at 4900 ms", waits)
	}
}

func TestSlowQueryWaitTotalsAddUpAcrossShapes(t *testing.T) {
	total, waits := readSlow(t, "node-a", waitLines+waitLines).WaitTotals()
	if total != 10000 {
		t.Errorf("total = %v ms, want 10000", total)
	}
	if len(waits) == 0 || waits[0].Name != "write concern" || waits[0].Millis != 7200 {
		t.Fatalf("waits = %+v, want write concern at 7200 ms", waits)
	}
	if waits[0].Meaning == "" {
		t.Error("the leading wait carries no explanation, and the explanation is the point of naming it")
	}
}

// The log-scale histogram's first bucket holds everything at or below 1, zeros included, and has no
// resolution inside it. Reading it as the bucket's lower edge reported 1 for a distribution of
// zeros, which put the median above the mean.
func TestQuantileOfMostlyZeroes(t *testing.T) {
	var numbers Numbers
	for range 90 {
		numbers.add(0)
	}
	for range 10 {
		numbers.add(5000)
	}
	if got := numbers.Quantile(0.50); got != 0 {
		t.Errorf("p50 = %v, want 0: nine in ten of these are zero", got)
	}
	if got := numbers.Quantile(0.95); got < 1000 {
		t.Errorf("p95 = %v, want the tail", got)
	}
	if p50, avg := numbers.Quantile(0.50), numbers.Avg(); p50 > avg {
		t.Errorf("p50 %v above the mean %v; no distribution does that", p50, avg)
	}
	if numbers.Min != 0 || numbers.Max != 5000 {
		t.Errorf("min %v max %v; bounds are exact and must not move", numbers.Min, numbers.Max)
	}
}

// TestWriteCountedOnce covers the pair of lines mongod writes for one slow write: the statement it
// ran, and the command the client sent. Both name the same collection once "$cmd" is resolved, and
// counting both reported one update as two — with the plan on one row and the time on another.
func TestWriteCountedOnce(t *testing.T) {
	when := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	s := NewSlowQueries()
	s.Add(slowQuery(when, "conn7",
		`{"type":"update","ns":"app.orders","planSummary":"IDHACK","durationMillis":90,"docsExamined":1,"keysExamined":1}`))
	s.Add(slowQuery(when.Add(time.Millisecond), "conn7",
		`{"type":"command","ns":"app.$cmd","command":{"update":"orders"},"durationMillis":100,"waitForWriteConcernDurationMillis":40}`))

	shapes := s.Shapes()
	if len(shapes) != 1 {
		t.Fatalf("a write was counted %d times: %+v", len(shapes), shapes)
	}
	shape := shapes[0]
	if shape.Count != 1 || shape.Namespace != "app.orders" || shape.Operation != "update" {
		t.Fatalf("paired into the wrong shape: %+v", shape)
	}
	// The envelope is what the client waited for; the statement is the only half that names a plan.
	if shape.Duration.Sum != 100 {
		t.Errorf("duration = %v, want the envelope's 100", shape.Duration.Sum)
	}
	if shape.Plan != "IDHACK" {
		t.Errorf("plan = %q, want the statement's IDHACK", shape.Plan)
	}
	if shape.WriteConcernWait.Sum != 40 {
		t.Errorf("write concern wait = %v, want the envelope's 40", shape.WriteConcernWait.Sum)
	}
	if shape.Docs.Sum != 1 || shape.Keys.Sum != 1 {
		t.Errorf("lost what the statement examined: %+v", shape)
	}
}

// TestStatementWithoutEnvelopeStillCounts guards the holding step: a statement whose command line
// never arrived — a truncated log, a different connection in between — must not be dropped.
func TestWritePairingDoesNotCrossNodes(t *testing.T) {
	when := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	statement := slowQuery(when, "conn7",
		`{"type":"update","ns":"app.orders","planSummary":"IDHACK","durationMillis":50}`)
	statement.Source = "node-a"
	envelope := slowQuery(when, "conn7",
		`{"type":"command","ns":"app.$cmd","command":{"update":"orders"},"durationMillis":60}`)
	envelope.Source = "node-b"
	s := NewSlowQueries()
	s.Add(statement)
	s.Add(envelope)
	var count int
	var total float64
	for _, shape := range s.Shapes() {
		count += shape.Count
		total += shape.Duration.Sum
	}
	if count != 2 || total != 110 {
		t.Fatalf("cross-node write pairing: count=%d duration=%v", count, total)
	}
}

func TestStatementWithoutEnvelopeStillCounts(t *testing.T) {
	when := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	s := NewSlowQueries()
	s.Add(slowQuery(when, "conn1",
		`{"type":"update","ns":"app.orders","planSummary":"IDHACK","durationMillis":50}`))
	s.Add(slowQuery(when, "conn2",
		`{"type":"command","ns":"app.$cmd","command":{"update":"orders"},"durationMillis":60}`))

	shapes := s.Shapes()
	if len(shapes) != 2 {
		t.Fatalf("a held statement was lost: %+v", shapes)
	}
	total := 0.0
	for _, shape := range shapes {
		total += shape.Duration.Sum
	}
	if total != 110 {
		t.Errorf("total = %v, want 110: neither line may be dropped", total)
	}
}

// TestFailedOperationsAreNamed covers the outcome. A duration says how long an operation ran, never
// whether it achieved anything, and the two are indistinguishable in a table ranked by time.
func TestFailedOperationsAreNamed(t *testing.T) {
	s := NewSlowQueries()
	for i := 0; i < 3; i++ {
		s.Add(slowQuery(time.Time{}, "conn1",
			`{"type":"command","ns":"app.$cmd","command":{"find":"orders"},"durationMillis":1000,"ok":0,"errName":"StaleConfig","errCode":13388}`))
	}
	s.Add(slowQuery(time.Time{}, "conn1",
		`{"type":"command","ns":"app.$cmd","command":{"find":"orders"},"durationMillis":500}`))

	shapes := s.Shapes()
	if len(shapes) != 1 {
		t.Fatalf("outcome split the shape: %+v", shapes)
	}
	shape := shapes[0]
	if shape.Count != 4 || shape.Failed != 3 || shape.FailedMillis != 3000 {
		t.Fatalf("counted %d of %d as failed (%v ms): %+v", shape.Failed, shape.Count, shape.FailedMillis, shape)
	}
	if name, count := shape.DominantError(); name != "StaleConfig" || count != 3 {
		t.Errorf("dominant error = %q ×%d, want StaleConfig ×3", name, count)
	}
	// Three of four is 75%, and a shape that had successes must never round to "all".
	if got := shape.Outcome(); got != "75% StaleConfig" {
		t.Errorf("outcome = %q, want 75%% StaleConfig", got)
	}
	executions, millis, byName := s.Failures()
	if executions != 3 || millis != 3000 || byName["StaleConfig"] != 3 {
		t.Errorf("failures = %d/%v/%v", executions, millis, byName)
	}
}

// TestOutcomeNeverRoundsToAll: 998 failures out of 1,000 is 99.8%, and the interesting fact about
// it is the two that got through.
func TestOutcomeNeverRoundsToAll(t *testing.T) {
	shape := Shape{Count: 1000, Failed: 998, Errors: map[string]int{"StaleConfig": 998}}
	if got := shape.Outcome(); got != "99% StaleConfig" {
		t.Errorf("outcome = %q, want 99%% StaleConfig", got)
	}
	shape.Failed = shape.Count
	shape.Errors["StaleConfig"] = shape.Count
	if got := shape.Outcome(); got != "all StaleConfig" {
		t.Errorf("outcome = %q, want all StaleConfig", got)
	}
	if got := (Shape{Count: 5}).Outcome(); got != "ok" {
		t.Errorf("outcome = %q, want ok", got)
	}
}

// TestInefficiencyIsAReadersMeasure: a write returns no documents, so examined-per-returned is its
// whole examined count. An update that found its row by _id and touched one document was screened
// as an index problem reading thousands of documents per document returned.
func TestInefficiencyIsAReadersMeasure(t *testing.T) {
	write := Shape{Operation: "update"}
	write.Docs.add(10000)
	if write.Reads() || write.Inefficiency() != 0 {
		t.Errorf("a write reported an inefficiency of %v", write.Inefficiency())
	}
	read := Shape{Operation: "find"}
	read.Docs.add(2000)
	read.Returned.add(0)
	if !read.Reads() || read.Inefficiency() != 2000 {
		t.Errorf("a read reported an inefficiency of %v, want 2000", read.Inefficiency())
	}
}

// A sample of a write is shown beside the shape it belongs to, so it has to be the same execution
// the shape counted: the envelope's duration, waits and command, not the statement half alone.
func TestSampledWritesAreThePairedExecution(t *testing.T) {
	when := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	for _, testcase := range []struct{ statement, envelope, operation string }{
		{`{"type":"update","ns":"app.orders","command":{"q":{"_id":1},"u":{"$set":{"a":1}}},"planSummary":"IDHACK","durationMillis":50}`,
			`{"type":"command","ns":"app.$cmd","command":{"update":"orders","writeConcern":{"w":"majority"}},"durationMillis":900,"waitForWriteConcernDurationMillis":840}`,
			"update"},
		{`{"type":"remove","ns":"app.orders","command":{"q":{"_id":1},"limit":1},"planSummary":"IDHACK","durationMillis":50}`,
			`{"type":"command","ns":"app.$cmd","command":{"delete":"orders","writeConcern":{"w":"majority"}},"durationMillis":900,"waitForWriteConcernDurationMillis":840}`,
			"delete"},
	} {
		t.Run(testcase.operation, func(t *testing.T) {
			s := NewSlowQueries()
			sampled := SampleShapes(ShapeFilter{}, 3)
			s.Sample(sampled)
			s.Add(slowQuery(when, "conn7", testcase.statement))
			s.Add(slowQuery(when, "conn7", testcase.envelope))
			shapes := s.Shapes()
			if len(shapes) != 1 || shapes[0].Operation != testcase.operation {
				t.Fatalf("shapes %+v, want one %s", shapes, testcase.operation)
			}
			lead := shapes[0]
			runs := sampled.Of(lead.Namespace, lead.Operation, lead.Plan, lead.ID)
			if len(runs) != 1 {
				t.Fatalf("sampled %d executions of the paired shape, want 1", len(runs))
			}
			run := runs[0]
			if run.Millis != 900 || run.Waits["write concern"] != 840 || run.Plan != "IDHACK" {
				t.Fatalf("sample is not the paired execution: %+v", run)
			}
			if !strings.Contains(string(run.Command), "writeConcern") {
				t.Fatalf("sample lost the envelope's command: %s", run.Command)
			}
		})
	}
}
