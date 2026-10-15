package analyze

import (
	"fmt"
	"testing"
	"time"
)

// buildLines are one index build followed from start to finish, plus the catalogue commands around
// it. The phases carry the build id, which is the only thing that ties them together when two
// builds run at once.
const buildLines = `{"t":{"$date":"2026-09-03T10:00:00Z"},"s":"I","c":"INDEX","id":20384,"ctx":"IndexBuildsCoordinator","msg":"Index build: starting","attr":{"buildUUID":{"uuid":{"$uuid":"0c1f7a9e-1111-2222-3333-444455556666"}},"namespace":"app.orders","properties":[{"name":"status_1"}]}}
{"t":{"$date":"2026-09-03T10:01:00Z"},"s":"I","c":"INDEX","id":20391,"ctx":"IndexBuildsCoordinator","msg":"Index build: collection scan done","attr":{"buildUUID":{"uuid":{"$uuid":"0c1f7a9e-1111-2222-3333-444455556666"}},"namespace":"app.orders"}}
{"t":{"$date":"2026-09-03T10:40:00Z"},"s":"I","c":"INDEX","id":20663,"ctx":"IndexBuildsCoordinator","msg":"Index build: done building","attr":{"buildUUID":{"uuid":{"$uuid":"0c1f7a9e-1111-2222-3333-444455556666"}},"namespace":"app.orders"}}
{"t":{"$date":"2026-09-03T11:00:00Z"},"s":"I","c":"COMMAND","id":20332,"ctx":"conn9","msg":"CMD: drop app.staging_2026"}
{"t":{"$date":"2026-09-03T11:01:00Z"},"s":"I","c":"COMMAND","id":20344,"ctx":"conn9","msg":"CMD: dropIndexes app.orders","attr":{"namespace":"app.orders","indexes":"old_idx"}}
`

func readDDL(t *testing.T, text string) *DDL {
	t.Helper()
	catalogue := NewDDL()
	feed(t, "node-a", text, catalogue)
	return catalogue
}

func TestDDLFollowsAnIndexBuildThroughItsPhases(t *testing.T) {
	builds := readDDL(t, buildLines).Builds()
	if len(builds) != 1 {
		t.Fatalf("read %d builds, want 1: the phases share a build id and are one build", len(builds))
	}
	build := builds[0]
	if build.Namespace != "app.orders" || build.Indexes != "status_1" {
		t.Errorf("build = %+v, want app.orders/status_1", build)
	}
	if build.Outcome != "completed" {
		t.Errorf("outcome = %q, want completed", build.Outcome)
	}
	// The duration is the thing no other section of the report can supply, and it is the reason to
	// follow the phases at all rather than count the lines.
	if build.Duration() != 40*time.Minute {
		t.Errorf("duration = %s, want 40m", build.Duration())
	}
	if build.Phases != 3 {
		t.Errorf("phases = %d, want 3", build.Phases)
	}
}

func TestDDLNamesTheCollectionACommandTouched(t *testing.T) {
	byKind := map[string]Change{}
	for _, change := range readDDL(t, buildLines).Changes() {
		byKind[change.Kind] = change
	}
	// "CMD: drop app.staging_2026" carries no attributes at all on some versions; the namespace is
	// in the message, and a drop with no collection beside it is a row that says nothing.
	if got := byKind["drop collection"].Namespace; got != "app.staging_2026" {
		t.Errorf("drop collection namespace = %q, want app.staging_2026", got)
	}
	if got := byKind["drop index"].Namespace; got != "app.orders" {
		t.Errorf("drop index namespace = %q, want app.orders", got)
	}
}

// A catalogue change that ran long enough to be logged as a slow query is read off the command
// document, which names itself whatever the server called the message.
func TestDDLReadsCatalogueCommandsOutOfSlowQueries(t *testing.T) {
	changes := readDDL(t,
		`{"t":{"$date":"2026-09-03T12:00:00Z"},"s":"I","c":"COMMAND","id":51803,"ctx":"conn4","msg":"Slow query","attr":{"type":"command","ns":"app.$cmd","command":{"createIndexes":"orders","indexes":[{"name":"a_1"}]},"durationMillis":90000}}`+"\n").Changes()
	if len(changes) != 1 {
		t.Fatalf("read %d changes, want 1", len(changes))
	}
	if changes[0].Kind != "create index" || changes[0].Namespace != "app.orders" {
		t.Errorf("change = %+v, want create index on app.orders", changes[0])
	}
	if changes[0].MaxMillis != 90000 {
		t.Errorf("slowest = %v ms, want 90000", changes[0].MaxMillis)
	}
}

// A build whose completion is in a file the bundle does not carry is not a completed build, and
// saying so is the difference between "this finished" and "this was still running".
func TestDDLLeavesAnUnfinishedBuildRunning(t *testing.T) {
	builds := readDDL(t,
		`{"t":{"$date":"2026-09-03T10:00:00Z"},"s":"I","c":"INDEX","id":20384,"ctx":"IndexBuildsCoordinator","msg":"Index build: starting","attr":{"buildUUID":{"uuid":{"$uuid":"aaaa1111-2222-3333-4444-555566667777"}},"namespace":"app.orders"}}`+"\n").Builds()
	if len(builds) != 1 || builds[0].Outcome != "running" {
		t.Fatalf("builds = %+v, want one still running", builds)
	}
	if builds[0].Duration() != 0 {
		t.Errorf("duration = %s; a build with no end has no duration to report", builds[0].Duration())
	}
}

// A server that rebuilds the same index at every startup logs no build id for it, so the only key
// those lines have is the namespace — and a namespace comes round again. Folding the rebuilds into
// one row reports a single build running from the first startup to the last. Each rebuild is its
// own build.
func TestDDLKeepsRepeatedRebuildsApart(t *testing.T) {
	const rebuild = `{"t":{"$date":"2026-09-03T%02d:00:00Z"},"s":"I","c":"INDEX","id":20345,"ctx":"initandlisten","msg":"Index build: starting","attr":{"namespace":"admin.system.version"}}
{"t":{"$date":"2026-09-03T%02d:05:00Z"},"s":"I","c":"INDEX","id":20346,"ctx":"initandlisten","msg":"Index build: done building index _id_ on ns admin.system.version","attr":{"namespace":"admin.system.version"}}
`
	text := ""
	for _, hour := range []int{10, 14, 18} {
		text += fmt.Sprintf(rebuild, hour, hour)
	}
	builds := readDDL(t, text).Builds()
	if len(builds) != 3 {
		t.Fatalf("read %d builds, want one per rebuild: %+v", len(builds), builds)
	}
	for _, build := range builds {
		if build.Outcome != "completed" {
			t.Errorf("build %+v did not complete", build)
		}
		if build.Duration() != 5*time.Minute {
			t.Errorf("build took %s, want 5m: a rebuild cannot last longer than the gap to the next one", build.Duration())
		}
	}
}
