package ftdc

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	reference "github.com/mongodb/ftdc"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// The decoder is only trustworthy if data written by somebody else's FTDC encoder comes back
// unchanged. Testing against an encoder of our own would only prove we are consistently wrong.
//
// The encoder here is github.com/mongodb/ftdc, a Go port, and it is not the server. On doubles the
// two disagree: the server truncates a NumberDouble to an integer before the delta stream, the port
// keeps the IEEE-754 bit pattern, and a decoder can follow one or the other. This package follows
// the server, because that is whose files it reads — see the note on columns() and
// TestCpuCountersAdvanceAtTheSpeedOfTheClock, which pins the same behaviour against real output.
// So no double is encoded below: the port cannot produce one this decoder is meant to agree with,
// and every other type it writes the way the server does.
func TestRoundTripThroughReferenceEncoder(t *testing.T) {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	const samples = 300
	type expectation struct {
		path   string
		values []float64
	}
	expected := []expectation{
		{path: "start"}, {path: "serverStatus/start"},
		{path: "serverStatus/mem/resident"},              // int32
		{path: "serverStatus/opcounters/insert"},         // int64 counter
		{path: "serverStatus/flag"},                      // bool
		{path: "serverStatus/wiredTiger/idle"},           // long zero run
		{path: "repl/optime/t"}, {path: "repl/optime/i"}, // raw seconds and increment
	}
	collector := reference.NewBatchCollector(samples)
	for i := range samples {
		at := start.Add(time.Duration(i) * time.Second)
		resident := int32(2048 + i) // int32
		inserts := int64(1_000_000 + 7*i)
		flag := i%3 == 0
		document := bson.D{
			{Key: "start", Value: at},
			{Key: "serverStatus", Value: bson.D{
				{Key: "start", Value: at},
				{Key: "mem", Value: bson.D{{Key: "resident", Value: resident}}},
				{Key: "opcounters", Value: bson.D{{Key: "insert", Value: inserts}}},
				{Key: "flag", Value: flag},
				{Key: "wiredTiger", Value: bson.D{{Key: "idle", Value: int64(0)}}},
			}},
			{Key: "repl", Value: bson.D{{Key: "optime", Value: primitive.Timestamp{T: uint32(at.Unix()), I: uint32(i)}}}},
		}
		if err := collector.Add(document); err != nil {
			t.Fatal(err)
		}
		millis := float64(at.UnixMilli())
		values := []float64{millis, millis, float64(resident), float64(inserts), boolValue(flag), 0,
			float64(at.Unix()), float64(i)}
		for column := range expected {
			expected[column].values = append(expected[column].values, values[column])
		}
	}
	payload, err := collector.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "metrics.2026-01-01T10-00-00Z-00000")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	file, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := file.Read(time.Time{}, time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Timestamps) != samples {
		t.Fatalf("decoded %d samples, want %d", len(decoded.Timestamps), samples)
	}
	for _, want := range expected {
		got, ok := decoded.Values[want.path]
		if !ok {
			t.Errorf("missing metric %q; decoded %d paths", want.path, len(decoded.Values))
			continue
		}
		for i := range want.values {
			if got[i] != want.values[i] {
				t.Errorf("%s sample %d = %v, want %v", want.path, i, got[i], want.values[i])
				break
			}
		}
	}
	if millis := decoded.Timestamps[1]; millis != start.Add(time.Second).UnixMilli() {
		t.Errorf("second timestamp = %d, want %d", millis, start.Add(time.Second).UnixMilli())
	}

}

// Schemas drift between chunks: a path missing from one chunk is a gap, never a repeated value.
func TestSchemaDriftLeavesGaps(t *testing.T) {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	first := chunkBytes(t, start, 5, func(at time.Time, i int) bson.D {
		return bson.D{{Key: "start", Value: at}, {Key: "serverStatus", Value: bson.D{
			{Key: "start", Value: at}, {Key: "a", Value: int64(i)}, {Key: "b", Value: int64(10 * i)}}}}
	})
	second := chunkBytes(t, start.Add(5*time.Second), 5, func(at time.Time, i int) bson.D {
		return bson.D{{Key: "start", Value: at}, {Key: "serverStatus", Value: bson.D{
			{Key: "start", Value: at}, {Key: "a", Value: int64(100 + i)}}}}
	})
	path := filepath.Join(t.TempDir(), "metrics.drift")
	if err := os.WriteFile(path, append(first, second...), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Chunks()) != 2 {
		t.Fatalf("indexed %d chunks, want 2", len(file.Chunks()))
	}
	decoded, err := file.Read(time.Time{}, time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := decoded.Values["serverStatus/b"]
	if len(b) != 10 {
		t.Fatalf("metric b has %d samples, want 10 aligned with the clock", len(b))
	}
	for i := 5; i < 10; i++ {
		if !math.IsNaN(b[i]) {
			t.Errorf("sample %d of a metric absent from the second chunk = %v, want a gap", i, b[i])
		}
	}
	if a := decoded.Values["serverStatus/a"]; a[5] != 100 {
		t.Errorf("metric present in both chunks lost its second chunk: %v", a[5])
	}
}

// Only the chunks a window touches are decoded; everything else stays compressed.
func TestWindowSelectsChunks(t *testing.T) {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	var file []byte
	for chunk := range 4 {
		at := start.Add(time.Duration(chunk) * 5 * time.Minute)
		file = append(file, chunkBytes(t, at, 10, func(at time.Time, i int) bson.D {
			return bson.D{{Key: "start", Value: at}, {Key: "serverStatus", Value: bson.D{
				{Key: "start", Value: at}, {Key: "a", Value: int64(i)}}}}
		})...)
	}
	path := filepath.Join(t.TempDir(), "metrics.window")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	indexed, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := indexed.Read(start.Add(11*time.Minute), start.Add(12*time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The third chunk covers 10:10 onwards; nothing else is needed for that window.
	if len(decoded.Timestamps) != 10 {
		t.Fatalf("decoded %d samples, want the 10 of one chunk", len(decoded.Timestamps))
	}
	if got := time.UnixMilli(decoded.Timestamps[0]).UTC(); !got.Equal(start.Add(10 * time.Minute)) {
		t.Errorf("first sample %s, want %s", got, start.Add(10*time.Minute))
	}
}

// A file ends at its last sample, not where its last chunk began: a chunk holds up to five minutes,
// and a span cut at the chunk's start says the node went quiet for them. The cached index says the
// same as a fresh one.
func TestAFileEndsAtItsLastSample(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	build := func(at time.Time, i int) bson.D {
		return bson.D{{Key: "start", Value: at}, {Key: "serverStatus", Value: bson.D{
			{Key: "start", Value: at}, {Key: "a", Value: int64(i)}}}}
	}
	file := append(chunkBytes(t, start, 10, build), chunkBytes(t, start.Add(5*time.Minute), 300, build)...)
	path := filepath.Join(t.TempDir(), "metrics.span")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	want := start.Add(5*time.Minute + 299*time.Second)
	for _, read := range []string{"indexed", "cached"} {
		indexed, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if first, last := indexed.Span(); !first.Equal(start) || !last.Equal(want) {
			t.Errorf("%s: span %s to %s, want %s to %s", read, first, last, start, want)
		}
	}
}

// A selector keeps the decode from materializing thousands of columns nobody asked about.
func TestSelectorKeepsOnlyRequestedMetrics(t *testing.T) {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "metrics.select")
	if err := os.WriteFile(path, chunkBytes(t, start, 20, func(at time.Time, i int) bson.D {
		return bson.D{{Key: "start", Value: at}, {Key: "serverStatus", Value: bson.D{
			{Key: "start", Value: at}, {Key: "a", Value: int64(i)}, {Key: "b", Value: int64(i * 2)}}}}
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := file.Read(time.Time{}, time.Time{}, func(path string) bool { return path == "serverStatus/b" })
	if err != nil {
		t.Fatal(err)
	}
	if _, present := decoded.Values["serverStatus/a"]; present {
		t.Error("unselected metric was materialized")
	}
	full, err := file.Read(time.Time{}, time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range full.Values["serverStatus/b"] {
		if decoded.Values["serverStatus/b"][i] != value {
			t.Fatalf("selected metric differs from a full decode at sample %d", i)
		}
	}
}

func chunkBytes(t *testing.T, start time.Time, samples int, build func(time.Time, int) bson.D) []byte {
	t.Helper()
	collector := reference.NewBatchCollector(samples)
	for i := range samples {
		if err := collector.Add(build(start.Add(time.Duration(i)*time.Second), i)); err != nil {
			t.Fatal(err)
		}
	}
	payload, err := collector.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func boolValue(flag bool) float64 {
	if flag {
		return 1
	}
	return 0
}

// FTDC records which server it came from, and that is what a stream should be called: a directory
// can be renamed, or hold two nodes side by side.
func TestHostComesFromTheData(t *testing.T) {
	for _, c := range []struct {
		what string
		doc  bson.M
		want string
	}{
		{"serverStatus.host", bson.M{"serverStatus": bson.M{"host": "mongo-a:27017"}}, "mongo-a:27017"},
		{"hostInfo fallback", bson.M{"hostInfo": bson.M{"system": bson.M{"hostname": "mongo-b"}}}, "mongo-b"},
		{"repl.me fallback", bson.M{"serverStatus": bson.M{"repl": bson.M{"me": "mongo-c:27017"}}}, "mongo-c:27017"},
		{"nothing to go on", bson.M{"serverStatus": bson.M{"uptime": 12}}, ""},
		{"an empty host is no host", bson.M{"serverStatus": bson.M{"host": ""}}, ""},
		{
			"the first place wins",
			bson.M{
				"serverStatus": bson.M{"host": "mongo-a", "repl": bson.M{"me": "mongo-z"}},
				"hostInfo":     bson.M{"system": bson.M{"hostname": "mongo-y"}},
			},
			"mongo-a",
		},
	} {
		raw, err := bson.Marshal(c.doc)
		if err != nil {
			t.Fatal(err)
		}
		if got := hostOf(bson.Raw(raw)); got != c.want {
			t.Errorf("%s: hostOf = %q, want %q", c.what, got, c.want)
		}
	}
}

// A chunk that did not survive the collection — a capture cut off mid-write, a byte lost copying it
// out of a container — is a hole in one node's series. The chunks around it still decode, and the
// read says what was lost: one damaged chunk used to end the whole answer, for every node in the
// bundle, with an "unexpected EOF" that named no file.
func TestADamagedChunkIsSkippedAndCounted(t *testing.T) {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	build := func(at time.Time, i int) bson.D {
		return bson.D{{Key: "start", Value: at}, {Key: "serverStatus", Value: bson.D{
			{Key: "start", Value: at}, {Key: "a", Value: int64(i)}}}}
	}
	var file []byte
	for chunk := range 3 {
		raw := chunkBytes(t, start.Add(time.Duration(chunk)*time.Minute), 10, build)
		if chunk == 1 {
			raw = damageChunk(t, raw)
		}
		file = append(file, raw...)
	}
	path := filepath.Join(t.TempDir(), "metrics.damaged")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	indexed, err := Open(path)
	if err != nil {
		t.Fatalf("a file with one damaged chunk could not be indexed: %v", err)
	}
	decoded, err := indexed.Read(time.Time{}, time.Time{}, nil)
	if err != nil {
		t.Fatalf("one damaged chunk out of three ended the read: %v", err)
	}
	if len(decoded.Timestamps) != 20 {
		t.Errorf("got %d samples, want the 20 of the two whole chunks", len(decoded.Timestamps))
	}
	if decoded.Skipped != 1 {
		t.Errorf("skipped %d chunks, want 1", decoded.Skipped)
	}
	if len(decoded.Damaged) != 1 || !strings.Contains(decoded.Damaged[0], "metrics.damaged") {
		t.Errorf("damage %v does not name the file it was in", decoded.Damaged)
	}
}

// Some of the chunks gone is a gap; all of them gone is nothing to draw, and the caller hears why
// rather than reading an empty chart as a quiet server.
func TestAWhollyDamagedFileIsAnError(t *testing.T) {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	raw := damageChunk(t, chunkBytes(t, start, 10, func(at time.Time, i int) bson.D {
		return bson.D{{Key: "start", Value: at}, {Key: "serverStatus", Value: bson.D{
			{Key: "start", Value: at}, {Key: "a", Value: int64(i)}}}}
	}))
	path := filepath.Join(t.TempDir(), "metrics.gone")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	indexed, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexed.Read(time.Time{}, time.Time{}, nil); err == nil {
		t.Fatal("a file whose every chunk is damaged read as an empty success")
	} else if !strings.Contains(err.Error(), "metrics.gone") {
		t.Errorf("error %q does not name the file", err)
	}
}

// damageChunk keeps a chunk's envelope intact — so it still indexes — and ruins the compressed
// payload inside it, which is what a truncated capture looks like from here.
func damageChunk(t *testing.T, chunk []byte) []byte {
	t.Helper()
	_, payload, ok := bson.Raw(chunk).Lookup("data").BinaryOK()
	if !ok {
		t.Fatal("chunk carries no binary payload")
	}
	at := bytes.Index(chunk, payload)
	if at < 0 || len(payload) < 16 {
		t.Fatalf("payload of %d bytes not found in the chunk", len(payload))
	}
	out := append([]byte(nil), chunk...)
	for i := at + 8; i < at+len(payload); i++ {
		out[i] = 0xff
	}
	return out
}

// A capture interrupted mid-write ends in half a document. The hours before that point are whole
// and are what the bundle was collected for: the file is indexed up to the damage and says where it
// stopped, instead of being refused entire and taking a node's metrics with it.
func TestATruncatedFileKeepsTheChunksBeforeTheCut(t *testing.T) {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	build := func(at time.Time, i int) bson.D {
		return bson.D{{Key: "start", Value: at}, {Key: "serverStatus", Value: bson.D{
			{Key: "start", Value: at}, {Key: "a", Value: int64(i)}}}}
	}
	var file []byte
	for chunk := range 3 {
		file = append(file, chunkBytes(t, start.Add(time.Duration(chunk)*time.Minute), 10, build)...)
	}
	last := chunkBytes(t, start.Add(3*time.Minute), 10, build)
	file = append(file, last[:len(last)/2]...) // the write stopped here

	path := filepath.Join(t.TempDir(), "metrics.cut")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	indexed, err := Open(path)
	if err != nil {
		t.Fatalf("a truncated file was refused entire: %v", err)
	}
	if len(indexed.Chunks()) != 3 {
		t.Errorf("indexed %d chunks, want the 3 whole ones", len(indexed.Chunks()))
	}
	if !strings.Contains(indexed.Cut, "cut short after 3 chunks") {
		t.Errorf("Cut = %q, does not say where the file stopped", indexed.Cut)
	}
	decoded, err := indexed.Read(time.Time{}, time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Timestamps) != 30 {
		t.Errorf("got %d samples, want 30", len(decoded.Timestamps))
	}
}

// Half a document and nothing else is a file with no history in it at all, and that is an error
// rather than an empty success.
func TestAFileThatIsOnlyACutIsAnError(t *testing.T) {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	raw := chunkBytes(t, start, 10, func(at time.Time, i int) bson.D {
		return bson.D{{Key: "start", Value: at}, {Key: "serverStatus", Value: bson.D{
			{Key: "start", Value: at}, {Key: "a", Value: int64(i)}}}}
	})
	path := filepath.Join(t.TempDir(), "metrics.stub")
	if err := os.WriteFile(path, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("a file holding no whole chunk opened successfully")
	}
}

// A double reaches the delta stream already truncated, so a column starts at the whole number and
// accumulates whole numbers. Keeping the IEEE-754 bit pattern instead leaves the first sample of
// every chunk right and every sample after it wrong, which is the shape of a bug nobody notices:
// read over a long enough window the endpoints still agree.
func TestADoubleIsCarriedAsItsTruncatedValue(t *testing.T) {
	document, err := bson.Marshal(bson.D{{Key: "cpu", Value: bson.D{{Key: "idle_ms", Value: 5000.5}}}})
	if err != nil {
		t.Fatal(err)
	}
	found, err := columns(bson.Raw(document))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].path != "cpu/idle_ms" {
		t.Fatalf("columns = %+v, want the one double", found)
	}
	if found[0].start != 5000 {
		t.Errorf("column starts at %d, want 5000: the fraction is gone before the file is written",
			found[0].start)
	}
	// Two seconds later on a 100-core host the counter has advanced by about 200,000.
	if got := found[0].value(5000 + 200000); got != 205000 {
		t.Errorf("after a delta of 200000 the value is %v, want 205000", got)
	}
	if bits := math.Float64bits(5000.5); found[0].start == bits {
		t.Error("the column kept the bit pattern, which is what the Go port does and the server does not")
	}
}

// Damage in the middle of a file is not the end of it. A capture with one unreadable stretch and
// whole chunks after it used to be indexed only up to the stretch, which discarded everything
// behind a single bad region. Whatever is whole after the damage is history, and the scan has to
// find it.
func TestMidFileDamageIsSteppedOver(t *testing.T) {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	build := func(at time.Time, i int) bson.D {
		return bson.D{{Key: "start", Value: at}, {Key: "serverStatus", Value: bson.D{
			{Key: "start", Value: at}, {Key: "a", Value: int64(i)}}}}
	}
	var file []byte
	for chunk := range 2 {
		file = append(file, chunkBytes(t, start.Add(time.Duration(chunk)*time.Minute), 10, build)...)
	}
	// A stretch the size of a chunk that is not one: what a bad write or a bad copy leaves.
	garbage := make([]byte, 4096)
	for i := range garbage {
		garbage[i] = byte(i*131 + 7)
	}
	file = append(file, garbage...)
	for chunk := 2; chunk < 4; chunk++ {
		file = append(file, chunkBytes(t, start.Add(time.Duration(chunk)*time.Minute), 10, build)...)
	}

	path := filepath.Join(t.TempDir(), "metrics.damaged")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	indexed, err := Open(path)
	if err != nil {
		t.Fatalf("a file damaged in the middle was refused: %v", err)
	}
	if got := len(indexed.Chunks()); got != 4 {
		t.Errorf("indexed %d chunks, want all 4: the two after the damage are whole", got)
	}
	if !strings.Contains(indexed.Cut, "skipped") || strings.Contains(indexed.Cut, "cut short") {
		t.Errorf("Cut = %q: it should say a stretch was skipped, and not that the file ended there", indexed.Cut)
	}
	decoded, err := indexed.Read(time.Time{}, time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Timestamps) != 40 {
		t.Errorf("got %d samples, want 40", len(decoded.Timestamps))
	}
}

// A whole document with no type is metadata an older server wrote, not damage. Reading forward it
// is skipped as it always was; only the search for where readable data resumes after damage insists
// on a type, because that is where garbage has to be told apart from a document.
func TestATypelessDocumentIsNotDamage(t *testing.T) {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	build := func(at time.Time, i int) bson.D {
		return bson.D{{Key: "start", Value: at}, {Key: "serverStatus", Value: bson.D{
			{Key: "start", Value: at}, {Key: "a", Value: int64(i)}}}}
	}
	typeless, err := bson.Marshal(bson.D{{Key: "_id", Value: start}, {Key: "doc", Value: bson.D{{Key: "host", Value: "node-a"}}}})
	if err != nil {
		t.Fatal(err)
	}
	file := append([]byte(nil), typeless...)
	file = append(file, chunkBytes(t, start, 10, build)...)
	path := filepath.Join(t.TempDir(), "metrics.typeless")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	indexed, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(indexed.Chunks()) != 1 || indexed.Cut != "" {
		t.Errorf("chunks=%d cut=%q: a typeless document was taken for damage", len(indexed.Chunks()), indexed.Cut)
	}
}
