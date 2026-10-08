package analyze

import (
	"fmt"
	"testing"
)

// startupLines are the handful of lines a server writes once, at the top of a log nobody scrolls
// back to. Everything a report can say about what it is looking at comes from these.
const startupLines = `{"t":{"$date":"2026-09-03T09:00:00Z"},"s":"I","c":"CONTROL","id":4615611,"ctx":"initandlisten","msg":"MongoDB starting","attr":{"pid":9,"port":27018,"dbPath":"/data/db","host":"shard-a-1"}}
{"t":{"$date":"2026-09-03T09:00:00Z"},"s":"I","c":"CONTROL","id":23403,"ctx":"initandlisten","msg":"Build Info","attr":{"buildInfo":{"version":"7.0.0-1","gitVersion":"deadbeef","modules":["enterprise"],"allocator":"tcmalloc"}}}
{"t":{"$date":"2026-09-03T09:00:00Z"},"s":"I","c":"CONTROL","id":51765,"ctx":"initandlisten","msg":"Operating System","attr":{"os":{"name":"Ubuntu 22.04 LTS","version":"Kernel 5.15.0"}}}
{"t":{"$date":"2026-09-03T09:00:00Z"},"s":"I","c":"CONTROL","id":21951,"ctx":"initandlisten","msg":"Options set by command line","attr":{"options":{"net":{"port":27018},"storage":{"dbPath":"/data/db"},"sharding":{"clusterRole":"shardsvr"},"replication":{"replSet":"shard0/a:27018,b:27018"},"security":{"authorization":"enabled"}}}}
{"t":{"$date":"2026-09-03T09:00:01Z"},"s":"I","c":"STORAGE","id":22270,"ctx":"initandlisten","msg":"Storage engine to use detected by data files","attr":{"dbpath":"/data/db","storageEngine":"wiredTiger"}}
{"t":{"$date":"2026-09-03T09:00:01Z"},"s":"I","c":"STORAGE","id":22315,"ctx":"initandlisten","msg":"Opening WiredTiger","attr":{"config":"create,cache_size=32768M,session_max=32000"}}
`

func readDeployment(t *testing.T, nodes ...nodeLog) *Deployment {
	t.Helper()
	deployment := NewDeployment()
	for _, node := range nodes {
		feed(t, node.name, node.text, deployment)
	}
	return deployment
}

func TestDeploymentReadsWhatWasRunning(t *testing.T) {
	processes := readDeployment(t, nodeLog{"shard-a", startupLines}).Processes()
	if len(processes) != 1 {
		t.Fatalf("read %d processes, want 1", len(processes))
	}
	process := processes[0]
	if process.Version != "7.0.0-1" {
		t.Errorf("version = %q, want 7.0.0-1", process.Version)
	}
	if process.Role != RoleShard {
		t.Errorf("role = %q, want %q", process.Role, RoleShard)
	}
	// The --replSet argument may carry a seed list; the set is the part before the slash.
	if process.ReplicaSet != "shard0" {
		t.Errorf("replica set = %q, want shard0", process.ReplicaSet)
	}
	if process.StorageEngine != "wiredTiger" {
		t.Errorf("storage engine = %q, want wiredTiger", process.StorageEngine)
	}
	// Nothing configured a cache size, so the only source for it is the config string WiredTiger
	// prints when it opens — which is where the default half-of-RAM figure lives.
	if process.CacheGB < 31.9 || process.CacheGB > 32.1 {
		t.Errorf("cache = %.2f GB, want about 32", process.CacheGB)
	}
	if process.OS == "" || process.Host != "shard-a-1" || process.Port != 27018 {
		t.Errorf("os %q, host %q, port %d", process.OS, process.Host, process.Port)
	}
	if process.Authorization != "enabled" {
		t.Errorf("authorization = %q, want enabled", process.Authorization)
	}
}

// A mongos writes the same Build Info as a mongod and nothing that says which it is except the name
// of the thread it starts on.
func TestDeploymentRecognisesMongosByItsStartupThread(t *testing.T) {
	processes := readDeployment(t, nodeLog{"router",
		`{"t":{"$date":"2026-09-03T09:00:00Z"},"s":"I","c":"CONTROL","id":23403,"ctx":"mongosMain","msg":"Build Info","attr":{"buildInfo":{"version":"7.0.0-1"}}}` + "\n"}).Processes()
	if len(processes) != 1 || processes[0].Role != RoleMongos {
		t.Fatalf("processes = %+v, want one mongos", processes)
	}
}

func TestDeploymentCountsRestartsAndUncleanShutdowns(t *testing.T) {
	deployment := readDeployment(t, nodeLog{"node-a", startupLines +
		`{"t":{"$date":"2026-09-03T10:00:00Z"},"s":"I","c":"CONTROL","id":20565,"ctx":"SignalHandler","msg":"Now exiting"}` + "\n" +
		`{"t":{"$date":"2026-09-03T10:05:00Z"},"s":"W","c":"STORAGE","id":22271,"ctx":"initandlisten","msg":"Detected unclean shutdown - Lock file is not empty","attr":{"lockFile":"/data/db/mongod.lock"}}` + "\n" +
		`{"t":{"$date":"2026-09-03T10:05:00Z"},"s":"I","c":"CONTROL","id":4615611,"ctx":"initandlisten","msg":"MongoDB starting","attr":{"pid":11,"port":27018,"host":"shard-a-1"}}` + "\n"})

	process := deployment.Processes()[0]
	if process.Starts != 2 || process.Exits != 1 || process.Unclean != 1 {
		t.Errorf("starts %d, exits %d, unclean %d; want 2, 1, 1", process.Starts, process.Exits, process.Unclean)
	}
	// One start is the process coming up; a second inside the same window is a restart, and every
	// counter in the bundle resets at it.
	if summary := deployment.Summarize(); summary.Restarts != 1 || summary.Unclean != 1 || !summary.Sharded {
		t.Errorf("summary = %+v, want 1 restart, 1 unclean, sharded", summary)
	}
}

// A log rotated past the last restart has no startup line at all, and the node is still worth a row.
func TestDeploymentLearnsTheSetFromReplicationLines(t *testing.T) {
	deployment, cluster := readDeployment(t, nodeLog{"node-a", replLines}), readCluster(t, nodeLog{"node-a", replLines})
	deployment.Learn(cluster)
	processes := deployment.Processes()
	if len(processes) != 1 || processes[0].ReplicaSet != "rs0" {
		t.Fatalf("processes = %+v, want one node in rs0", processes)
	}
	if processes[0].Version != "" {
		t.Errorf("version = %q; a bundle with no startup line cannot know it", processes[0].Version)
	}
}

// A mongos never writes "MongoDB starting", so counting starts from that line alone reported every
// router in a bundle as having never started. Build Info is written by both binaries, once per
// start — and mongod stamps both lines with the same millisecond, so taking either has to collapse
// the pair or it counts each start twice.
func TestStartsAreCountedOncePerStartupWhicheverLineSurvives(t *testing.T) {
	const buildInfo = `{"t":{"$date":"%s"},"s":"I","c":"CONTROL","id":23403,"ctx":"%s","msg":"Build Info","attr":{"buildInfo":{"version":"%s"}}}`
	const greeting = `{"t":{"$date":"%s"},"s":"I","c":"CONTROL","id":4615611,"ctx":"initandlisten","msg":"MongoDB starting","attr":{"pid":9,"host":"a"}}`

	deployment := readDeployment(t,
		// A mongod: both lines, same millisecond, twice over.
		nodeLog{"node-a", sprintfLine(greeting, "2026-09-03T09:00:00.100Z") +
			sprintfLine(buildInfo, "2026-09-03T09:00:00.100Z", "initandlisten", "5.0.0") +
			sprintfLine(greeting, "2026-09-03T12:00:00.500Z") +
			sprintfLine(buildInfo, "2026-09-03T12:00:00.500Z", "initandlisten", "6.0.0")},
		// A mongos: Build Info only.
		nodeLog{"router", sprintfLine(buildInfo, "2026-09-03T09:05:00Z", "mongosMain", "5.0.0") +
			sprintfLine(buildInfo, "2026-09-03T13:05:00Z", "mongosMain", "5.0.0")})

	byRole := map[string]Process{}
	for _, process := range deployment.Processes() {
		byRole[process.Role] = process
	}
	mongod, mongos := byRole[RoleMongod], byRole[RoleMongos]
	if mongod.Starts != 2 {
		t.Errorf("mongod starts = %d, want 2: the greeting and the build info are one startup", mongod.Starts)
	}
	if mongos.Starts != 2 {
		t.Errorf("mongos starts = %d, want 2: a router writes no greeting at all", mongos.Starts)
	}
	if mongos.LastStartAt.Format("15:04") != "13:05" {
		t.Errorf("mongos last start = %s, want 13:05", mongos.LastStartAt.Format("15:04"))
	}

	// The version is the one from the latest startup, not from whichever rotation arrived first.
	if mongod.Version != "6.0.0" {
		t.Errorf("mongod version = %q, want 6.0.0: the newest Build Info wins", mongod.Version)
	}
	if got := mongod.Previous(); len(got) != 1 || got[0] != "5.0.0" {
		t.Errorf("previous versions = %v, want [5.0.0]", got)
	}
	if !mongod.Upgraded() {
		t.Error("a process that ran two versions does not report it")
	}
	if mongos.Upgraded() {
		t.Error("a process that ran one version reports a change")
	}
}

func sprintfLine(format string, args ...any) string {
	values := make([]any, len(args))
	for i, arg := range args {
		values[i] = arg
	}
	return fmt.Sprintf(format, values...) + "\n"
}

// "5.0.0, 6.0.0" in one cell says both were seen. In order, with times, it says whether the
// upgrade finished, stalled halfway, or was rolled back — three different support cases.
func TestVersionHistoryReadsAsAChangeLog(t *testing.T) {
	const buildInfo = `{"t":{"$date":"%s"},"s":"I","c":"CONTROL","id":23403,"ctx":"initandlisten","msg":"Build Info","attr":{"buildInfo":{"version":"%s"}}}`
	deployment := readDeployment(t,
		nodeLog{"rolled-back", sprintfLine(buildInfo, "2026-09-03T06:00:00Z", "5.0.0") +
			sprintfLine(buildInfo, "2026-09-03T07:00:00Z", "5.0.0") + // a restart, not a change
			sprintfLine(buildInfo, "2026-09-03T15:00:00Z", "6.0.0") +
			sprintfLine(buildInfo, "2026-09-04T05:00:00Z", "5.0.0")},
		nodeLog{"upgraded", sprintfLine(buildInfo, "2026-09-03T06:00:00Z", "5.0.0") +
			sprintfLine(buildInfo, "2026-09-03T15:00:00Z", "6.0.0")})

	bySource := map[string]Process{}
	for _, process := range deployment.Processes() {
		bySource[process.Source] = process
	}

	rolled, upgraded := bySource["rolled-back"], bySource["upgraded"]
	// Four starts, three versions in order: the repeat is a restart and not a change.
	if got := rolled.VersionChanges(); len(got) != 3 ||
		got[0].Version != "5.0.0" || got[1].Version != "6.0.0" || got[2].Version != "5.0.0" {
		t.Fatalf("version changes = %+v, want 5.0.0 → 6.0.0 → 5.0.0", got)
	}
	if rolled.Starts != 4 {
		t.Errorf("starts = %d, want 4: a repeat of the same version is still a start", rolled.Starts)
	}
	if !rolled.Downgraded() {
		t.Error("a version reappearing after a different one is not reported as a rollback")
	}
	if upgraded.Downgraded() {
		t.Error("a completed upgrade is reported as a rollback")
	}
	// The current version is the one from the last startup, whichever rotation it was read from.
	if rolled.Version != "5.0.0" || upgraded.Version != "6.0.0" {
		t.Errorf("current versions: rolled %q, upgraded %q", rolled.Version, upgraded.Version)
	}
}

// rotationBanner is what a server writes at the top of every rotated log: the same lines it wrote
// at startup, from the thread that handled the signal.
const rotationBanner = `{"t":{"$date":"2026-09-05T08:00:00Z"},"s":"I","c":"CONTROL","id":20721,"ctx":"SignalHandler","msg":"Process Details","attr":{"pid":"1000","port":27018,"architecture":"64-bit","host":"node-a.example.com"}}
{"t":{"$date":"2026-09-05T08:00:00Z"},"s":"I","c":"CONTROL","id":20722,"ctx":"SignalHandler","msg":"Node is a member of a replica set","attr":{"config":{"_id":"rs0","version":9,"members":[{"host":"node-a.example.com:27018"}]}}}
{"t":{"$date":"2026-09-05T08:00:00Z"},"s":"I","c":"CONTROL","id":23403,"ctx":"SignalHandler","msg":"Build Info","attr":{"buildInfo":{"version":"8.0.0-1","gitVersion":"deadbeef"}}}
{"t":{"$date":"2026-09-05T08:00:00Z"},"s":"I","c":"CONTROL","id":51765,"ctx":"SignalHandler","msg":"Operating System","attr":{"os":{"name":"Ubuntu 24.04 LTS"}}}
`

// TestRotationIsNotARestart covers the banner a log rotation writes. It is the startup lines again,
// so a node whose log rotates on a schedule was reported as a node that restarts on that schedule
// — and because each rotation is a file and a process was read per file, each file came out as a
// mongod of its own, with restarts between them that never happened.
func TestRotationIsNotARestart(t *testing.T) {
	processes := readDeployment(t, nodeLog{"node-a", rotationBanner}).Processes()
	if len(processes) != 1 {
		t.Fatalf("read %d processes, want 1", len(processes))
	}
	process := processes[0]
	if process.Starts != 0 {
		t.Errorf("counted %d starts from a rotation banner", process.Starts)
	}
	if !process.LastStartAt.IsZero() {
		t.Errorf("a rotation set a last start time of %s", process.LastStartAt)
	}
	// Everything the banner says is still read: on a deployment that has been up for months it is
	// the only place any of it appears.
	if process.Version != "8.0.0-1" || process.ReplicaSet != "rs0" {
		t.Errorf("lost what the banner said: %+v", process)
	}
	if process.Host != "node-a.example.com" || process.Port != 27018 {
		t.Errorf("host and port = %q:%d, want node-a.example.com:27018", process.Host, process.Port)
	}
}
