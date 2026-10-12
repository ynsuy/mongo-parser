package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	reference "github.com/mongodb/ftdc"
	"go.mongodb.org/mongo-driver/bson"
)

// pidba sends the parser's JSON to a model whole, in pages when it is large, and its tests check that
// on a report this test writes from a real run over a small invented bundle. Run with
// -update-agent-fixture after a change to the JSON.
var updateAgentFixture = flag.Bool("update-agent-fixture", false, "rewrite pidba's parser report fixture")

const agentFixture = "../../../pidba/tests/fixtures/parser-report.json"

func TestAgentFixtureIsTheParsersCurrentReport(t *testing.T) {
	if _, err := os.Stat(filepath.Dir(filepath.Dir(agentFixture))); err != nil {
		t.Skip("pidba is not beside the parser")
	}
	got := reportJSON(t, inventedBundle(t))
	if *updateAgentFixture {
		if err := os.MkdirAll(filepath.Dir(agentFixture), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(agentFixture, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(agentFixture)
	if err != nil {
		t.Fatalf("%v; run go test ./internal/cli -run TestAgentFixture -update-agent-fixture", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the report JSON changed; review it, then run go test ./internal/cli -run TestAgentFixture -update-agent-fixture and npm test in pidba")
	}
}

// reportJSON runs the report over a bundle and returns its JSON with everything that differs from
// run to run — paths, build stamp, the rendered text — taken out.
func reportJSON(t *testing.T, root string) []byte {
	t.Helper()
	t.Setenv("MONGO_LOG_PARSER_CACHE", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = write
	done := make(chan []byte)
	go func() {
		data, _ := io.ReadAll(read)
		done <- data
	}()
	code := Run([]string{"report", root, "--json", "--no-chart"})
	os.Stdout = stdout
	write.Close()
	out := <-done
	if code != 0 {
		t.Fatalf("report exited %d", code)
	}
	var envelope map[string]any
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["render"] = ""
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		data = bytes.ReplaceAll(data, []byte(resolved), []byte("/bundle"))
	}
	return append(bytes.ReplaceAll(data, []byte(root), []byte("/bundle")), '\n')
}

// inventedBundle is one node: forty minutes of FTDC in which host CPU, iowait and blocked processes
// rise together for six minutes, and a log with slow queries, a paired write among them.
func inventedBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	start := time.Date(2026, 5, 6, 7, 0, 0, 0, time.UTC)
	dir := filepath.Join(root, "node-a", "diagnostic.data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const minutes = 40
	collector := reference.NewBatchCollector(minutes * 60)
	var user, system, idle, iowait int64
	for second := range minutes * 60 {
		at := start.Add(time.Duration(second) * time.Second)
		busy, wait, blocked := int64(2000), int64(100), int64(0) // of 10000 ms a second across ten cores
		if minute := second / 60; minute >= 20 && minute < 26 {
			busy, wait, blocked = 8500, 1200, 8
		}
		user += busy * 8 / 10
		system += busy * 2 / 10
		iowait += wait
		idle += 10000 - busy - wait
		if err := collector.Add(bson.D{{Key: "start", Value: at},
			{Key: "systemMetrics", Value: bson.D{{Key: "start", Value: at}, {Key: "cpu", Value: bson.D{
				{Key: "user_ms", Value: user}, {Key: "nice_ms", Value: int64(0)}, {Key: "system_ms", Value: system},
				{Key: "idle_ms", Value: idle}, {Key: "iowait_ms", Value: iowait}, {Key: "irq_ms", Value: int64(0)},
				{Key: "softirq_ms", Value: int64(0)}, {Key: "steal_ms", Value: int64(0)},
				{Key: "procs_blocked", Value: blocked}}}}}}); err != nil {
			t.Fatal(err)
		}
	}
	payload, err := collector.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metrics.2026-05-06T07-00-00Z-00000"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	line := func(at time.Time, ctx, attr string) {
		fmt.Fprintf(&log, `{"t":{"$date":"%s"},"s":"I","c":"COMMAND","id":51803,"ctx":"%s","msg":"Slow query","attr":%s}`+"\n",
			at.Format("2006-01-02T15:04:05.000Z"), ctx, attr)
	}
	for i := range 3 {
		at := start.Add(time.Duration(21*60+i*30) * time.Second)
		line(at, fmt.Sprintf("conn%d", 10+i), `{"type":"command","ns":"shop.items","command":{"find":"items","filter":{"sku":"x"}},"planSummary":"COLLSCAN","queryHash":"0A1B2C3D","docsExamined":50000,"nreturned":1,"durationMillis":`+fmt.Sprint(400+i*300)+`}`)
	}
	at := start.Add(22 * time.Minute)
	line(at, "conn20", `{"type":"update","ns":"shop.orders","command":{"q":{"_id":1},"u":{"$set":{"s":1}}},"planSummary":"IDHACK","keysExamined":1,"docsExamined":1,"durationMillis":40}`)
	line(at, "conn20", `{"type":"command","ns":"shop.$cmd","command":{"update":"orders","writeConcern":{"w":"majority"}},"durationMillis":2600,"waitForWriteConcernDurationMillis":2550}`)
	if err := os.WriteFile(filepath.Join(root, "node-a", "mongod.log"), []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
