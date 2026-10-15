package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ynsuy/mongo-parser/internal/ftdc"
	"github.com/ynsuy/mongo-parser/internal/source"
	"github.com/ynsuy/mongo-parser/internal/store"
)

func TestFTDCCopiesRequireMatchingContents(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "on disk", true: "archived"}[archived], func(t *testing.T) {
			in := &ingester{ftdcDir: t.TempDir(), ftdc: map[string]*ftdcGroup{}, seenFTDC: map[string]string{}}
			add := func(dir, name, data string) {
				t.Helper()
				entry := &source.Entry{Path: dir + "/" + name, Dir: dir, Name: name, Size: int64(len(data)), Kind: source.FTDC}
				if !archived {
					entry.OnDisk = filepath.Join(in.ftdcDir, dir, name)
				}
				if err := in.visit(entry, strings.NewReader(data)); err != nil {
					t.Fatal(err)
				}
			}
			// Same name and size, different bytes: both nodes must survive.
			add("node-a", "metrics.same", "node-a")
			add("node-b", "metrics.same", "node-b")
			// A renamed copy is still a copy.
			add("copy", "metrics.renamed", "node-a")
			if len(in.ftdc) != 2 || in.ftdc["node-a"] == nil || in.ftdc["node-b"] == nil {
				t.Fatalf("different contents were lost or a copy was retained: %+v", in.ftdc)
			}
			if in.sources[0].Note != "" || in.sources[1].Note != "" || !strings.Contains(in.sources[2].Note, "an exact copy of node-a/metrics.same") {
				t.Fatalf("incorrect duplicate notes: %+v", in.sources)
			}
		})
	}
}

// An archive and its unpacked copy are one diagnostic.data. Each file is read once, from whichever
// copy came first, so the files end up spread over both; they are still one stream, not two that
// each hold part of the day.
func TestFTDCSplitAcrossCopiesIsOneStream(t *testing.T) {
	in := &ingester{ftdcDir: t.TempDir(), ftdc: map[string]*ftdcGroup{}, seenFTDC: map[string]string{}}
	add := func(dir, name string) {
		t.Helper()
		entry := &source.Entry{Path: dir + "/" + name, Dir: dir, Name: name, Size: int64(len(name)), Kind: source.FTDC}
		if err := in.visit(entry, strings.NewReader(name)); err != nil {
			t.Fatal(err)
		}
	}
	add("node-a.zip!diagnostic.data", "metrics.1")
	add("node-a/diagnostic.data", "metrics.1")
	add("node-a/diagnostic.data", "metrics.2")
	add("node-b/diagnostic.data", "metrics.3")
	rows := in.indexFTDC()
	if len(rows) != 2 {
		t.Fatalf("got %d streams, want node-a's and node-b's: %+v", len(rows), rows)
	}
	if rows[0].Origin != "node-a.zip!diagnostic.data" || len(rows[0].Paths) != 2 || len(rows[1].Paths) != 1 {
		t.Errorf("node-a's two files are not one stream: %+v", rows)
	}
}

func TestFTDCReadFailureDoesNotRegisterCopy(t *testing.T) {
	in := &ingester{ftdcDir: t.TempDir(), ftdc: map[string]*ftdcGroup{}, seenFTDC: map[string]string{}}
	want := errors.New("interrupted FTDC input")
	entry := &source.Entry{Path: "node/metrics.test", Dir: "node", Name: "metrics.test", Kind: source.FTDC}
	err := in.visit(entry, io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(want)))
	if !errors.Is(err, want) || len(in.seenFTDC) != 0 || len(in.ftdc) != 0 {
		t.Fatalf("incomplete contents registered: err=%v hashes=%v streams=%v", err, in.seenFTDC, in.ftdc)
	}
	// Nothing of it may stay on disk either: a later read of the directory would find it.
	_ = filepath.Walk(in.ftdcDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			t.Errorf("incomplete file left behind: %s", path)
		}
		return nil
	})
}

// The server's own truncation note follows attr in an export's message; it is not part of attr.
func TestExportedMessageWithTruncationNote(t *testing.T) {
	msg, attrText := splitMessage(`Slow query, attr={'ns': 'a.b'}, truncated={'command': {'type': 'object', 'size': 9000}}`)
	if msg != "Slow query" {
		t.Fatalf("message %q", msg)
	}
	out, rest, err := pyPrefix(attrText)
	if err != nil || out != `{"ns":"a.b"}` || !strings.HasPrefix(strings.TrimSpace(rest), ", truncated=") {
		t.Fatalf("attr %s rest %q err %v", out, rest, err)
	}
}

func TestCSVExportUsesTheServersTimeNotTheCollectors(t *testing.T) {
	csv := "\"@timestamp\";message;severity;\"instance-id\";\"source_timestamp\"\n" +
		"\"2026-01-01 12:00:05.000\";\"Slow query, attr={'durationMillis': 5}\";I;\"pod-0\";\"2026-01-01 12:00:01.500\"\n"
	var got []store.Record
	_, note, _, err := ReadCSVLog(strings.NewReader(csv), func(_ string, r store.Record) { got = append(got, r) })
	if err != nil || len(got) != 1 {
		t.Fatalf("err %v rows %d", err, len(got))
	}
	if got[0].TS%60000 != 1500 {
		t.Errorf("time %d: read the collector's @timestamp instead of source_timestamp", got[0].TS)
	}
	if !strings.Contains(note, "source_timestamp") {
		t.Errorf("note %q does not say which column the times came from", note)
	}
}

func stream(self, host, pod string, port int) streamRow {
	return streamRow{Stats: Stats{Lines: 1, Identity: Identity{Self: self, Host: host, Port: port, Pod: pod}}}
}

// Three datacenters, one pod name each: the FTDC streams all report the same short host. They are
// three machines and must stay three nodes.
func TestSameShortNameInThreeDatacentersStaysThreeNodes(t *testing.T) {
	const port = ":27017"
	full := func(dc string) string { return "rs0-0.svc." + dc + ".example.net" + port }
	streams := []streamRow{stream(full("dc1"), "", "rs0-0", 0)}
	var ftdcs []ftdcRow
	for _, dc := range []string{"dc1", "dc2", "dc3"} {
		ftdcs = append(ftdcs, ftdcRow{Origin: dc, Identity: ftdc.Identity{Host: "rs0-0" + port, Me: full(dc)}, Chunks: 1})
	}
	nodes := resolveNodes(streams, ftdcs)
	if len(nodes) != 3 {
		var names []string
		for _, n := range nodes {
			names = append(names, n.Name)
		}
		t.Fatalf("got %d nodes %v, want 3", len(nodes), names)
	}
	labels := map[string]bool{}
	for _, n := range nodes {
		labels[n.Label] = true
	}
	for _, want := range []string{"rs0-0@dc1", "rs0-0@dc2", "rs0-0@dc3"} {
		if !labels[want] {
			t.Errorf("labels %v lack %s", labels, want)
		}
	}
}

// Two sites whose domains also end differently: the site word names them, not the top-level domain
// that happens to differ too.
func TestTheSiteNamesANodeNotItsTopLevelDomain(t *testing.T) {
	ftdcs := []ftdcRow{
		{Origin: "a", Identity: ftdc.Identity{Host: "rs0-1.svc.prod.dc1.local:27017"}, Chunks: 1},
		{Origin: "b", Identity: ftdc.Identity{Host: "rs0-1.svc.prod.dc2.example.net:27017"}, Chunks: 1},
	}
	labels := map[string]bool{}
	for _, n := range resolveNodes(nil, ftdcs) {
		labels[n.Label] = true
	}
	if !labels["rs0-1@dc1"] || !labels["rs0-1@dc2"] {
		t.Fatalf("labels %v, want rs0-1@dc1 and rs0-1@dc2", labels)
	}
}

// One machine: the log says its unqualified hostname, FTDC says the same; they are one node.
func TestUnqualifiedNamesJoinWhenUnambiguous(t *testing.T) {
	streams := []streamRow{stream("", "ip-10-0-0-5", "", 27017)}
	ftdcs := []ftdcRow{{Identity: ftdc.Identity{Host: "ip-10-0-0-5:27017"}, Chunks: 1}}
	if nodes := resolveNodes(streams, ftdcs); len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
}

// serverStatus.host leaves the default port out, so FTDC says "db1" where the log says "db1:27017".
// They are one node. A second mongod on the same machine reports its port, and stays its own node.
func TestFTDCOnTheDefaultPortJoinsItsLog(t *testing.T) {
	streams := []streamRow{stream("", "db1", "", 27017), stream("", "db1", "", 27018)}
	ftdcs := []ftdcRow{
		{Origin: "a", Identity: ftdc.Identity{Host: "db1"}, Chunks: 1},
		{Origin: "b", Identity: ftdc.Identity{Host: "db1:27018"}, Chunks: 1},
	}
	nodes := resolveNodes(streams, ftdcs)
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want two: one per port", len(nodes))
	}
	for _, n := range nodes {
		if len(n.members) != 2 {
			t.Errorf("node %s has %d streams, want its log and its FTDC", n.Name, len(n.members))
		}
	}
}

// A collector writes one archive per host and the rotated logs inside it never restate who they
// belong to. The host's name is on the archive, and the FTDC beside it says the same name, so the
// logs and the metrics of that machine are one node rather than two halves that cannot be compared.
func TestRotatedLogsJoinTheNodeTheirPathNames(t *testing.T) {
	logs := []streamRow{
		{Dir: "db1_logs.tar.gz!logs/mongodb", Name: "mongodb.log-20260101.gz", Stats: Stats{Lines: 1}},
		{Dir: "db2_logs.tar.gz!logs/mongodb", Name: "mongodb.log-20260101.gz", Stats: Stats{Lines: 1}},
	}
	ftdcs := []ftdcRow{
		{Origin: "db1_diagnostic.data.tar.gz!diagnostic.data", Chunks: 1,
			Identity: ftdc.Identity{Host: "db1.example.com:27017"}},
		{Origin: "db2_diagnostic.data.tar.gz!diagnostic.data", Chunks: 1,
			Identity: ftdc.Identity{Host: "db2.example.com:27017"}},
	}
	nodes := resolveNodes(logs, ftdcs)
	if len(nodes) != 2 {
		var names []string
		for _, n := range nodes {
			names = append(names, n.Name)
		}
		t.Fatalf("got %d nodes %v, want 2", len(nodes), names)
	}
	for _, n := range nodes {
		if len(n.members) != 2 {
			t.Errorf("node %s has %d streams, want its log and its FTDC", n.Name, len(n.members))
		}
		if n.fallback {
			t.Errorf("node %s is still named after a file", n.Name)
		}
		named := false
		for _, e := range n.Evidence {
			named = named || strings.Contains(e, "its path names")
		}
		if !named {
			t.Errorf("node %s does not say the name came from the path: %v", n.Name, n.Evidence)
		}
	}
}

// A path is weak evidence and only counts where it settles the question. One directory holding both
// nodes' names says nothing about which wrote the log, and a name that merely starts another node's
// name is not that node.
func TestAPathThatNamesTwoNodesNamesNeither(t *testing.T) {
	both := []streamRow{{Dir: "case/mongo1_and_mongo11_logs", Name: "mongodb.log", Stats: Stats{Lines: 1}}}
	prefix := []streamRow{{Dir: "case/mongo11_logs", Name: "mongodb.log", Stats: Stats{Lines: 1}}}
	ftdcs := []ftdcRow{
		{Origin: "case/a", Chunks: 1, Identity: ftdc.Identity{Host: "mongo1.example.com:27017"}},
		{Origin: "case/b", Chunks: 1, Identity: ftdc.Identity{Host: "mongo11.example.com:27017"}},
	}
	if nodes := resolveNodes(both, ftdcs); len(nodes) != 3 {
		t.Errorf("got %d nodes, want the log kept apart from both", len(nodes))
	}
	nodes := resolveNodes(prefix, ftdcs)
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want the log on mongo11", len(nodes))
	}
	for _, n := range nodes {
		if strings.HasPrefix(n.Name, "mongo1.") && len(n.members) != 1 {
			t.Errorf("mongo1 took a log written in a directory named after mongo11")
		}
	}
}

// A collector that writes each node's log beside its diagnostic.data has named the node in the only
// place it could, when neither file says whose it is. The FTDC is that directory's node, not a node
// called diagnostic.data; two FTDC streams in one directory settle nothing.
func TestUnnamedFTDCJoinsTheUnnamedLogBesideIt(t *testing.T) {
	logs := []streamRow{
		{Dir: "bundle/node-a", Name: "mongod.log", Stats: Stats{Lines: 1}},
		{Dir: "bundle/node-b", Name: "mongod.log", Stats: Stats{Lines: 1}},
	}
	ftdcs := []ftdcRow{
		{Origin: "bundle/node-a/diagnostic.data", Chunks: 1},
		{Origin: "bundle/node-b/diagnostic.data", Chunks: 1},
		{Origin: "bundle/node-b/mongodb/diagnostic.data", Chunks: 1},
	}
	var got []string
	for _, n := range resolveNodes(logs, ftdcs) {
		got = append(got, fmt.Sprintf("%s:%d", n.Name, len(n.members)))
	}
	want := []string{"node-a:2", "node-b:1", "node-b:1", "node-b/mongod:1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("nodes %v, want %v", got, want)
	}
}

// A crash writes several lines in one millisecond; the line before the silence is the last of them.
func TestSilenceStartsAfterTheLastLineOfAMillisecond(t *testing.T) {
	in := &ingester{blocks: make(chan blockOut, 8)}
	b := in.newBuilder(1)
	b.add(store.Record{TS: 1000, Msg: "Fatal assertion", Sev: "F"})
	b.add(store.Record{TS: 1000, Msg: "***aborting after fassert() failure", Sev: "F"})
	b.add(store.Record{TS: 1000 + 16*60*1000, Msg: "***** SERVER RESTARTED *****", Sev: "I"})
	if len(b.gaps) != 1 {
		t.Fatalf("gaps %d", len(b.gaps))
	}
	if b.gaps[0].Before.Slot != 1 {
		t.Errorf("silence opens at slot %d, want 1 (the last line of that millisecond)", b.gaps[0].Before.Slot)
	}
}

// An export that splits a line's attributes into columns instead of printing "msg, attr={...}":
// the attr document in one column, the command lifted out into another, the duration in a third.
// Read as a message alone, every one of these lines is a slow query with no namespace, no command
// and a duration of zero — which is what `slow` reported for a whole bundle of them.
func TestExportWithAttributesInColumns(t *testing.T) {
	csv := `"@timestamp";component;"context_";duration;"instance-id";message;namespace;query;"query_meta";replicaset;severity;"source_timestamp"` + "\n" +
		`"2026-01-01 12:00:00.500";COMMAND;conn100;100;"rs0-1";"Slow query";example-ns;"{""update"": ""orders"", ""$db"": ""test""}";"{""type"": ""command"", ""ns"": ""test.$cmd"", ""durationMillis"": 100}";"rs0";I;"2026-01-01 12:00:00.000"` + "\n"
	var got []store.Record
	if _, _, _, err := ReadCSVLog(strings.NewReader(csv), func(_ string, r store.Record) { got = append(got, r) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d rows, want 1", len(got))
	}
	var attr struct {
		NS       string          `json:"ns"`
		Duration int             `json:"durationMillis"`
		Command  json.RawMessage `json:"command"`
	}
	if err := json.Unmarshal([]byte(got[0].Attr()), &attr); err != nil {
		t.Fatalf("attributes are not a document: %v (%s)", err, got[0].Attr())
	}
	if attr.NS != "test.$cmd" {
		t.Errorf("ns = %q, want the one in the query_meta column", attr.NS)
	}
	if attr.Duration != 100 {
		t.Errorf("durationMillis = %d, want 100", attr.Duration)
	}
	if !strings.Contains(string(attr.Command), `"update"`) {
		t.Errorf("command = %s, want the document from the query column", attr.Command)
	}
	// The k8s namespace column is a pod's namespace, not a collection's, and must not become ns.
	if attr.NS == "example-ns" {
		t.Error("the export's Kubernetes namespace was read as the MongoDB namespace")
	}
}

// A row whose attribute column is not a document is counted as unparsed rather than quietly
// becoming an empty attr: the difference is whether a zero is a measurement or a miss.
func TestExportWithUnreadableAttributeColumn(t *testing.T) {
	csv := `"@timestamp";message;"query_meta";"instance-id"` + "\n" +
		`"2026-01-01 12:00:00.500";"Slow query";"not a document";"rs0-1"` + "\n"
	streams, _, _, err := ReadCSVLog(strings.NewReader(csv), func(string, store.Record) {})
	if err != nil {
		t.Fatal(err)
	}
	if streams["rs0-1"].Stats.Unparsed != 1 {
		t.Errorf("unparsed = %d, want 1", streams["rs0-1"].Stats.Unparsed)
	}
}

// Some exports render the line the way a human reads it and fold the plan into the message, so a
// slow query arrives as "Slow query, planSummary=EXPRESS_IXSCAN { _id: 1 },EXPRESS_UPDATE".
// Everything downstream matches the message exactly, so left alone these are not slow queries at
// all — and they are the only ones that carry a plan.
func TestCSVExportKeepsThePlanOutOfTheMessage(t *testing.T) {
	csv := "\"source_timestamp\";message;severity;\"instance-id\";\"query_meta\"\n" +
		"\"2026-01-01 12:00:00.000\";\"Slow query, planSummary=EXPRESS_IXSCAN { _id: 1 },EXPRESS_UPDATE\";I;\"pod-0\";" +
		"\"{\"\"type\"\": \"\"update\"\", \"\"ns\"\": \"\"test.orders\"\", \"\"durationMillis\"\": 100}\"\n"
	var got []store.Record
	if _, _, _, err := ReadCSVLog(strings.NewReader(csv), func(_ string, r store.Record) { got = append(got, r) }); err != nil || len(got) != 1 {
		t.Fatalf("err %v rows %d", err, len(got))
	}
	if got[0].Msg != "Slow query" {
		t.Fatalf("message %q: the slow-query fold matches the message exactly, so this line is invisible", got[0].Msg)
	}
	if !strings.Contains(string(got[0].Attr()), `"planSummary":"EXPRESS_IXSCAN { _id: 1 },EXPRESS_UPDATE"`) {
		t.Errorf("attr %s dropped the plan the message carried", got[0].Attr())
	}
}

// When the attribute column already names the plan, that one wins: it is what the server wrote,
// and the message is a rendering of it.
func TestCSVExportPrefersThePlanTheAttributesCarry(t *testing.T) {
	csv := "\"source_timestamp\";message;severity;\"instance-id\";\"query_meta\"\n" +
		"\"2026-01-01 12:00:00.000\";\"Slow query, planSummary=COLLSCAN\";I;\"pod-0\";" +
		"\"{\"\"planSummary\"\": \"\"IXSCAN { a: 1 }\"\", \"\"durationMillis\"\": 7}\"\n"
	var got []store.Record
	if _, _, _, err := ReadCSVLog(strings.NewReader(csv), func(_ string, r store.Record) { got = append(got, r) }); err != nil || len(got) != 1 {
		t.Fatalf("err %v rows %d", err, len(got))
	}
	if !strings.Contains(string(got[0].Attr()), `"planSummary":"IXSCAN { a: 1 }"`) {
		t.Errorf("attr %s: the message overwrote the attribute the server wrote", got[0].Attr())
	}
}

// A stretched cluster's export names the same pod in every datacenter and says which one in its dc
// column. Each row's pod joins the node of that name whose domain is that datacenter.
func TestExportPodsJoinTheirDatacentersNode(t *testing.T) {
	csv := "\"source_timestamp\";message;severity;\"instance-id\";dc\n" +
		"\"2026-01-01 12:00:01.000\";\"Slow query, attr={'durationMillis': 5}\";I;\"rs0-0\";dc4\n" +
		"\"2026-01-01 12:00:02.000\";\"Slow query, attr={'durationMillis': 6}\";I;\"rs0-0\";dc5\n"
	streams, _, _, err := ReadCSVLog(strings.NewReader(csv), func(string, store.Record) {})
	if err != nil || len(streams) != 2 || streams["rs0-0@dc4"] == nil || streams["rs0-0@dc5"] == nil {
		t.Fatalf("streams %v err %v, want one per datacenter", streams, err)
	}
	var rows []streamRow
	for _, key := range []string{"rs0-0@dc4", "rs0-0@dc5"} {
		rows = append(rows, streamRow{Instance: key, Stats: streams[key].Stats})
	}
	ftdcs := []ftdcRow{
		{Origin: "a", Identity: ftdc.Identity{Host: "rs0-0.svc.prod.dc4.local:31017"}, Chunks: 1},
		{Origin: "b", Identity: ftdc.Identity{Host: "rs0-0.svc.prod.dc5.example.net:31017"}, Chunks: 1},
	}
	nodes := resolveNodes(rows, ftdcs)
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(nodes))
	}
	for _, n := range nodes {
		if len(n.members) != 2 {
			t.Fatalf("node %s has %d members, want its pod's rows and its FTDC", n.Label, len(n.members))
		}
		site := n.members[0].stream
		if site == nil {
			site = n.members[1].stream
		}
		if !strings.Contains(n.Name, "."+site.Stats.Identity.Site+".") {
			t.Errorf("rows from %s joined %s", site.Stats.Identity.Site, n.Name)
		}
	}
}

// An export whose time column is local time is found out by the times the server wrote inside its
// lines, and moved back to UTC.
func TestExportInLocalTimeIsMovedToUTC(t *testing.T) {
	csv := "\"@timestamp\";message;severity;\"instance-id\"\n"
	for i := range 6 {
		csv += fmt.Sprintf("\"2026-01-01 12:0%d:00.100\";\"x, attr={'lastCommittedWall': {'$date': '2026-01-01T10:0%d:00.000Z'}}\";I;\"pod-0\"\n", i, i)
	}
	_, note, offset, err := ReadCSVLog(strings.NewReader(csv), func(string, store.Record) {})
	if err != nil || offset != 2*3600*1000 {
		t.Fatalf("offset %d err %v, want 2h", offset, err)
	}
	if !strings.Contains(note, "2h ahead of UTC") {
		t.Errorf("note %q does not say so", note)
	}
}

// The current log is also the head of the archive that rotated it. Its lines are counted once.
func TestOverlappingLogsAreReadOnce(t *testing.T) {
	root := t.TempDir()
	line := func(i int) string {
		return fmt.Sprintf(`{"t":{"$date":"2026-01-01T00:00:%02d.000Z"},"s":"I","c":"NETWORK","id":22943,"ctx":"listener","msg":"Connection accepted","attr":{"n":%d}}`, i, i) + "\n"
	}
	var archive, current string
	for i := range 40 {
		archive += line(i)
		if i < 25 {
			current += line(i)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "mongod.log-20260102"), []byte(archive), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mongod.log"), []byte(current), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Run(root, filepath.Join(t.TempDir(), "store"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var lines, streams int
	if err := st.DB.QueryRow(`SELECT sum(lines) FROM block`).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	_ = st.DB.QueryRow(`SELECT count(*) FROM stream`).Scan(&streams)
	if lines != 40 || streams != 1 {
		t.Fatalf("%d lines in %d streams, want the archive's 40 in one", lines, streams)
	}
}
