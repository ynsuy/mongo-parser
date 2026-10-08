package analyze

import (
	"strings"
	"testing"
)

const replLines = `{"t":{"$date":"2026-09-03T10:00:00Z"},"s":"I","c":"REPL","id":21358,"ctx":"conn1","msg":"Replica set state transition","attr":{"newState":"SECONDARY","oldState":"STARTUP2","replicaSetName":"rs0"}}
{"t":{"$date":"2026-09-03T10:01:00Z"},"s":"I","c":"ELECTION","id":23980,"ctx":"conn1","msg":"Starting an election, since we've seen no PRIMARY in election timeout period"}
{"t":{"$date":"2026-09-03T10:01:01Z"},"s":"I","c":"ELECTION","id":23981,"ctx":"conn1","msg":"Election succeeded, assuming primary role"}
{"t":{"$date":"2026-09-03T10:01:02Z"},"s":"I","c":"REPL","id":21358,"ctx":"conn1","msg":"Replica set state transition","attr":{"newState":"PRIMARY","oldState":"SECONDARY"}}
{"t":{"$date":"2026-09-03T10:20:00Z"},"s":"I","c":"REPL","id":21106,"ctx":"conn1","msg":"Stepping down from primary"}
{"t":{"$date":"2026-09-03T10:30:00Z"},"s":"I","c":"SHARDING","id":22080,"ctx":"conn2","msg":"Error while doing moveChunk"}
{"t":{"$date":"2026-09-03T10:31:00Z"},"s":"I","c":"NETWORK","id":22943,"ctx":"listener","msg":"Connection accepted"}
`

// nodeLog is one node's log, the way a bundle holds it: a name and the lines written under it.
type nodeLog struct{ name, text string }

// readNodes folds every node's lines into a single cluster, which is what one pass over a bundle
// does.
func readNodes(t *testing.T, nodes ...nodeLog) *Cluster {
	t.Helper()
	cluster := NewCluster()
	for _, n := range nodes {
		feed(t, n.name, n.text, cluster)
	}
	return cluster
}

// readCluster is readNodes for the tests that are about nodes with events of their own.
func readCluster(t *testing.T, nodes ...nodeLog) *Cluster {
	t.Helper()
	cluster := readNodes(t, nodes...)
	if len(cluster.Nodes()) == 0 {
		t.Fatal("no nodes reported")
	}
	return cluster
}

func TestClusterCountsElectionsAndTransitions(t *testing.T) {
	cluster := readNodes(t, nodeLog{"node-a", replLines})

	nodes := cluster.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("reported %d nodes, want 1", len(nodes))
	}
	node := nodes[0]
	if node.ReplicaSet != "rs0" {
		t.Errorf("replica set = %q, want rs0", node.ReplicaSet)
	}
	if node.States["PRIMARY"] != 1 || node.States["SECONDARY"] != 1 {
		t.Errorf("states = %v, want one transition into each", node.States)
	}
	if len(node.Transitions) != 2 || node.Transitions[0].To != "SECONDARY" || node.Transitions[1].From != "SECONDARY" {
		t.Errorf("transitions = %+v", node.Transitions)
	}
	if node.ElectionsCalled != 1 || node.ElectionsWon != 1 {
		t.Errorf("elections called %d, won %d; want 1 and 1", node.ElectionsCalled, node.ElectionsWon)
	}
	if node.StepDowns != 1 {
		t.Errorf("step downs = %d, want 1", node.StepDowns)
	}
	if node.MigrationErrors != 1 {
		t.Errorf("migration errors = %d, want 1", node.MigrationErrors)
	}
	if called, won := cluster.Elections(); called != 1 || won != 1 {
		t.Errorf("cluster elections = %d called, %d won", called, won)
	}
}

// A node that only logged ordinary traffic has nothing to say in a replication summary.
func TestClusterSkipsQuietNodes(t *testing.T) {
	cluster := readNodes(t, nodeLog{"node-b",
		`{"t":{"$date":"2026-09-03T10:00:00Z"},"s":"I","c":"NETWORK","id":22943,"ctx":"l","msg":"Connection accepted"}` + "\n"})

	if got := len(cluster.Nodes()); got != 0 {
		t.Errorf("reported %d nodes, want none", got)
	}
}

// One pass over the logs has to feed every aggregate, or a report costs as many scans as sections.
func TestFanoutFeedsEverySink(t *testing.T) {
	groups, queries, cluster := NewGrouper(), NewSlowQueries(), NewCluster()
	feed(t, "node-a", replLines+lines, groups.Only(""), queries, cluster)

	if len(groups.Groups()) == 0 {
		t.Error("the grouper saw nothing")
	}
	if len(queries.Shapes()) != 1 {
		t.Errorf("slow queries saw %d shapes, want the one in the fixture", len(queries.Shapes()))
	}
	if len(cluster.Nodes()) != 1 {
		t.Errorf("the cluster summary saw %d nodes, want 1", len(cluster.Nodes()))
	}
}

// noiseLines are the messages a healthy node writes that read like replication trouble: the dry run
// before an election, the oplog note a new primary writes after winning, WiredTiger's recovery step
// and the rollback id every node keeps at startup. None of them is an election or a rollback.
const noiseLines = `{"t":{"$date":"2026-09-03T10:00:58Z"},"s":"I","c":"ELECTION","id":23973,"ctx":"conn1","msg":"Conducting a dry run election to see if we could be elected"}
{"t":{"$date":"2026-09-03T10:00:59Z"},"s":"I","c":"ELECTION","id":23974,"ctx":"conn1","msg":"Dry election run succeeded, running for election"}
{"t":{"$date":"2026-09-03T10:01:03Z"},"s":"I","c":"REPL","id":21331,"ctx":"conn1","msg":"Logging transition to primary to oplog on stepup"}
{"t":{"$date":"2026-09-03T10:01:04Z"},"s":"I","c":"STORAGE","id":22430,"ctx":"initandlisten","msg":"recovery was completed successfully and took 2ms, including 1ms for the log replay, 0ms for the rollback to stable, and 0ms for the checkpoint."}
{"t":{"$date":"2026-09-03T10:01:05Z"},"s":"I","c":"REPL","id":21599,"ctx":"initandlisten","msg":"Searching for local Rollback ID document"}
{"t":{"$date":"2026-09-03T10:01:06Z"},"s":"I","c":"REPL","id":21600,"ctx":"initandlisten","msg":"Initialized the rollback ID"}
`

func TestClusterIgnoresElectionAndRollbackNoise(t *testing.T) {
	cluster := readCluster(t, nodeLog{"node-a", replLines + noiseLines})

	node := cluster.Nodes()[0]
	if node.ElectionsCalled != 1 || node.ElectionsWon != 1 {
		t.Errorf("elections called %d, won %d; want 1 and 1: a dry run and the stepup oplog note are not elections",
			node.ElectionsCalled, node.ElectionsWon)
	}
	if node.Rollbacks != 0 {
		t.Errorf("rollbacks %d; want 0: recovery and the rollback id are not replication rollbacks", node.Rollbacks)
	}
}

// A node that truly rolls back reports it by entering the ROLLBACK state.
func TestClusterCountsRollbackFromStateTransition(t *testing.T) {
	cluster := readCluster(t, nodeLog{"node-a",
		`{"t":{"$date":"2026-09-03T11:00:00Z"},"s":"I","c":"REPL","id":21358,"ctx":"conn1","msg":"Replica set state transition","attr":{"newState":"ROLLBACK","oldState":"SECONDARY"}}` + "\n"})

	if node := cluster.Nodes()[0]; node.Rollbacks != 1 {
		t.Errorf("rollbacks %d; want 1", node.Rollbacks)
	}
}

// TestPeerStateOutlivesItsOwnLog covers the only evidence there is about a node after its own log
// ends. Logs in one bundle rarely end together: a node's log can stop with it a SECONDARY while a
// peer goes on to watch it enter PRIMARY.
func TestPeerStateOutlivesItsOwnLog(t *testing.T) {
	cluster := readCluster(t,
		nodeLog{"shard-a",
			`{"t":{"$date":"2026-09-08T03:10:00Z"},"s":"I","c":"REPL","id":21358,"ctx":"ReplCoord-0","msg":"Replica set state transition","attr":{"newState":"SECONDARY","oldState":"RECOVERING"}}` + "\n"},
		nodeLog{"shard-b",
			`{"t":{"$date":"2026-09-08T03:05:00Z"},"s":"I","c":"REPL","id":21358,"ctx":"ReplCoord-0","msg":"Replica set state transition","attr":{"newState":"SECONDARY","oldState":"RECOVERING"}}` + "\n" +
				`{"t":{"$date":"2026-09-08T04:30:00Z"},"s":"I","c":"REPL","id":21215,"ctx":"ReplCoord-2","msg":"Member is in new state","attr":{"hostAndPort":"shard-a.example:27017","newState":"PRIMARY"}}` + "\n"})

	peers := cluster.PeerStates()
	if len(peers) != 1 {
		t.Fatalf("read %d peer states, want 1: %+v", len(peers), peers)
	}
	if peers[0].Member != "shard-a.example:27017" || peers[0].State != "PRIMARY" {
		t.Fatalf("peer state = %+v", peers[0])
	}
	// The peer's own log wrote it, and the finding has to be able to say so.
	if !strings.Contains(peers[0].SeenBy, "shard-b") {
		t.Errorf("seen by %q, want shard-b's log", peers[0].SeenBy)
	}
	// Every node's own last state is SECONDARY, which is what used to be read as "no primary".
	for _, node := range cluster.Nodes() {
		if node.LastState != "SECONDARY" {
			t.Errorf("%s last state = %q, want SECONDARY", node.Source, node.LastState)
		}
		if node.LastStateAt.IsZero() {
			t.Errorf("%s did not record when its log ran out", node.Source)
		}
	}
}

// TestPeerStateKeepsOnlyTheLatest: a member seen in PRIMARY and then in SECONDARY is a member that
// is not primary any more, and keeping both would answer "was there a primary" with "yes, once".
func TestPeerStateKeepsOnlyTheLatest(t *testing.T) {
	// Read without the helper that insists on nodes: a log that only reports on other members is not
	// itself a node with events, so Nodes() rightly returns none and the peer states are the whole of
	// what it said.
	cluster := readNodes(t, nodeLog{"node",
		`{"t":{"$date":"2026-09-08T04:00:00Z"},"s":"I","c":"REPL","id":21215,"ctx":"ReplCoord-2","msg":"Member is in new state","attr":{"hostAndPort":"a:27017","newState":"PRIMARY"}}` + "\n" +
			`{"t":{"$date":"2026-09-08T05:00:00Z"},"s":"I","c":"REPL","id":21215,"ctx":"ReplCoord-2","msg":"Member is in new state","attr":{"hostAndPort":"a:27017","newState":"SECONDARY"}}` + "\n"})

	if len(cluster.Nodes()) != 0 {
		t.Errorf("a log that only watched others became %d rows of its own", len(cluster.Nodes()))
	}
	peers := cluster.PeerStates()
	if len(peers) != 1 || peers[0].State != "SECONDARY" {
		t.Fatalf("peer states = %+v, want one SECONDARY", peers)
	}
}
