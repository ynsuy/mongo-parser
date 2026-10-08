package analyze

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// memberStates are the states a replica set member reports moving between.
var memberStates = map[string]bool{
	"STARTUP": true, "STARTUP2": true, "PRIMARY": true, "SECONDARY": true, "RECOVERING": true,
	"ROLLBACK": true, "REMOVED": true, "ARBITER": true, "DOWN": true, "UNKNOWN": true,
	// Not a replica set state: the process was shut down and there was nothing to be in a state.
	// A stopped member and a member that failed to be elected are the same absence of a PRIMARY and
	// completely different reports, and without this the first was filed as the second.
	StateStopped: true,
}

// StateStopped is a member whose own log says its process exited. Every other state here is one
// MongoDB names; this one is the absence of the process that would have a state.
const StateStopped = "STOPPED"

// Transition is one member state change, as the node itself reported it.
type Transition struct {
	At   time.Time `json:"at"`
	From string    `json:"from"`
	To   string    `json:"to"`
}

// Node is what the logs of one node say about its role in the set.
type Node struct {
	Source      string         `json:"source"`
	ReplicaSet  string         `json:"replica_set,omitempty"`
	Transitions []Transition   `json:"transitions"`
	States      map[string]int `json:"states"`

	ElectionsCalled  int `json:"elections_called"`
	ElectionsWon     int `json:"elections_won"`
	ElectionsLost    int `json:"elections_lost"`
	StepDowns        int `json:"step_downs"`
	SyncSourceChange int `json:"sync_source_changes"`
	Rollbacks        int `json:"rollbacks"`

	InitialSyncs      int `json:"initial_syncs"`
	HeartbeatFailures int `json:"heartbeat_failures"`

	ChunkMigrations int `json:"chunk_migrations"`
	MigrationErrors int `json:"migration_errors"`
	BalancerRounds  int `json:"balancer_rounds"`

	// LastState is the state this node was in when its log ran out, which is the one state a
	// reader actually asks about: a set with no PRIMARY at the end of the window is a different
	// report from one that merely held an election in the middle of it.
	LastState string `json:"last_state,omitempty"`
	// LastStateAt is when that state was entered. Logs in one bundle rarely end together — a
	// rotation can cut one node off well before another — so "the end of the window" is a
	// different moment for every node, and a claim about the set has to say which.
	LastStateAt time.Time `json:"last_state_at,omitzero"`
	// Truncated is set once this node logged more transitions than are kept. A timeline drawn from
	// a truncated history would show the last state it managed to record running to the end of the
	// window, which is a flapping member drawn as a steady one — so whoever draws it has to be able
	// to say the history is partial rather than quietly show half of it.
	Truncated bool `json:"transitions_truncated,omitempty"`
}

// PeerState is what one node's log says about another member, read off the heartbeat line the
// server writes when a member it is watching changes state.
//
// It is the only evidence there is about a node after its own log ends. When logs are collected at
// different moments, one node's log can stop with it a SECONDARY while a peer's log goes on to
// record that same node becoming PRIMARY; read alone, the first says the set had no primary.
type PeerState struct {
	Member string    `json:"member"`
	State  string    `json:"state"`
	At     time.Time `json:"at"`
	// SeenBy is the log that reported it, so the claim can be traced to a line.
	SeenBy string `json:"seen_by"`
}

// PeerStates is the latest state each member was seen in by any log in the bundle, newest first.
func (c *Cluster) PeerStates() []PeerState {
	out := make([]PeerState, 0, len(c.peers))
	for _, seen := range c.peers {
		out = append(out, seen)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].Member < out[j].Member
	})
	return out
}

// maxReports bounds the heartbeat history kept for one member. A set whose members are flapping
// logs a report every few seconds from every peer, and the timeline they feed is drawn at one
// column per bin: past a few hundred changes the picture is "it flapped", which the count already
// says.
const maxReports = 2000

// maxTransitions bounds what one node keeps: a flapping member can log thousands, and the count
// in States already says how often it happened.
const maxTransitions = 100

// Migration is what the chunks of one namespace did over the window. Per node it is a number with
// no handle on it; per namespace it names the collection whose balancing is the work.
type Migration struct {
	Namespace   string    `json:"ns"`
	Moves       int       `json:"moves"`
	Failures    int       `json:"failures"`
	First, Last time.Time `json:"-"`
}

// Cluster folds replication, election and sharding events out of a log stream.
type Cluster struct {
	nodes      map[string]*Node
	migrations map[string]*Migration
	// peers is the latest state each member was seen in by anybody, keyed by host and port.
	peers map[string]PeerState
	// The evidence a state timeline is built from; see replication.go for what each is for.
	reports   map[string][]stateEvent // member host:port -> every heartbeat report about it
	selves    map[string]identity     // log file -> the member it identified itself as
	roster    map[string]string       // member host:port -> the set whose config names it
	truncated map[string]bool         // member host:port -> its heartbeat history ran past the cap
	lifecycle map[string][]stateEvent // log file -> when its process started and stopped
	claimed   map[string]string       // log file -> the host:port it says it runs on, if a roster agrees
	coverage  map[string]coverage     // log file -> the stretch of time it spans
	// unstable is when the set was not simply running: elections, step downs, rollbacks,
	// heartbeats that failed, members changing state. Kept as a history rather than a total
	// because the question it answers is "was this happening at the same time as that", and a
	// total cannot answer it.
	unstable Timeline
}

func NewCluster() *Cluster {
	return &Cluster{nodes: map[string]*Node{}, migrations: map[string]*Migration{},
		peers: map[string]PeerState{}, reports: map[string][]stateEvent{}, selves: map[string]identity{},
		roster: map[string]string{}, truncated: map[string]bool{}, lifecycle: map[string][]stateEvent{},
		claimed:  map[string]string{},
		coverage: map[string]coverage{}, unstable: Timeline{}}
}

// Unstable is the history of the set being anything other than steady.
func (c *Cluster) Unstable() Timeline { return c.unstable }

type replAttributes struct {
	NewState   string `json:"newState"`
	OldState   string `json:"oldState"`
	ReplicaSet string `json:"replicaSetName"`
	Member     string `json:"hostAndPort"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Config     struct {
		Set     string `json:"_id"`
		Members []struct {
			Host string `json:"host"`
		} `json:"members"`
	} `json:"config"`
	SyncSource string `json:"syncSource"`
	Namespace  string `json:"namespace"`
	NS         string `json:"ns"`
}

// Add counts one record against the node whose log it came from.
func (c *Cluster) Add(record Record) {
	// Before anything is judged relevant: how far this log reaches. A state holds until the next
	// line only while there is a log to write that line, and every "unknown" on a timeline is the
	// far end of one of these.
	if !record.At.IsZero() {
		span := c.coverage[record.Source]
		if span.first.IsZero() || record.At.Before(span.first) {
			span.first = record.At
		}
		if record.At.After(span.last) {
			span.last = record.At
		}
		c.coverage[record.Source] = span
	}

	message := strings.ToLower(record.Msg)
	relevant := strings.Contains(message, "state transition") || strings.Contains(message, "new state") ||
		message == "process details" || strings.HasPrefix(message, "mongodb starting") ||
		message == "shutting down" || message == "now exiting" || message == "mongodb starting" ||
		strings.Contains(message, "now in state") || strings.Contains(message, "found self") ||
		strings.Contains(message, "config in use") || strings.Contains(message, "member of a replica set") ||
		strings.Contains(message, "election") ||
		strings.Contains(message, "step") || strings.Contains(message, "sync source") ||
		strings.Contains(message, "initial sync") || strings.Contains(message, "heartbeat") ||
		strings.Contains(message, "chunk") ||
		strings.Contains(message, "migrat") || strings.Contains(message, "balancer")
	if !relevant {
		return
	}
	node := c.nodes[record.Source]
	if node == nil {
		node = &Node{Source: record.Source, States: map[string]int{}}
		c.nodes[record.Source] = node
	}

	// Order matters here and is not alphabetical: every branch below is a substring test, and the
	// words overlap. "Balancer moving chunk" is a migration and says "balancer"; a failed moveChunk
	// says both "migration" and "fail". The specific reading comes first in every such pair, and a
	// new branch belongs above the one that would otherwise swallow it.
	switch {
	// A clean exit and the start that follows it are the only lines that say the process was not
	// there. A long silence in the middle of a bundle is a maintenance window as often as it is an
	// outage, and nothing else in a log tells the two apart.
	case message == "shutting down", message == "now exiting":
		c.lifecycle[record.Source] = append(c.lifecycle[record.Source],
			stateEvent{at: record.At, state: StateStopped, evidence: Observed, seenBy: record.Source})
	case message == "mongodb starting":
		c.lifecycle[record.Source] = append(c.lifecycle[record.Source],
			stateEvent{at: record.At, state: "STARTUP", evidence: Observed, seenBy: record.Source})
	// Where "Found self in config" is absent — 7.0 does not write it, and a log rotated past the
	// last restart may not hold it either — the host and port the process states are a candidate
	// for the same join. Only a candidate: on a cloud host the machine's own name and the name the
	// set knows it by are different strings, so it is used only if the roster contains it.
	case message == "process details", strings.HasPrefix(message, "mongodb starting"):
		var attributes replAttributes
		record.DecodeAttributes(&attributes)
		if attributes.Host != "" && attributes.Port != 0 {
			c.claimed[record.Source] = attributes.Host + ":" + strconv.Itoa(attributes.Port)
		}
	// "Found self in config" is the only line that ties a log file to the name its peers know it
	// by outright. A process reports its machine's hostname at startup; the set knows it by
	// whatever the replica set config says, and on a cloud host those are never the same string.
	case strings.Contains(message, "found self"):
		var attributes replAttributes
		record.DecodeAttributes(&attributes)
		if attributes.Member != "" {
			self := c.selves[record.Source]
			self.hostAndPort = attributes.Member
			if node.ReplicaSet != "" {
				self.set = node.ReplicaSet
			}
			c.selves[record.Source] = self
		}
	// The config names every member of the set, including the ones this bundle has no log for and
	// the ones nothing could reach. Leaving those out of the roster is how a missing shard goes
	// unnoticed in a report about a set that has one.
	case strings.Contains(message, "config in use"), strings.Contains(message, "member of a replica set"):
		var attributes replAttributes
		record.DecodeAttributes(&attributes)
		if attributes.Config.Set == "" {
			return
		}
		node.ReplicaSet = attributes.Config.Set
		if self, joined := c.selves[record.Source]; joined && self.set == "" {
			self.set = attributes.Config.Set
			c.selves[record.Source] = self
		}
		for _, member := range attributes.Config.Members {
			if member.Host != "" {
				c.roster[member.Host] = attributes.Config.Set
			}
		}
	// Two heartbeat lines say what a peer saw: "Member is in new state" puts the state in an
	// attribute, and "Member is now in state RS_DOWN" spells it in the message — and that second
	// one is the only line in a log that ever says a member went away.
	case strings.Contains(message, "new state"), strings.Contains(message, "now in state"):
		var attributes replAttributes
		record.DecodeAttributes(&attributes)
		state := attributes.NewState
		if state == "" {
			state = reportedState(record.Msg)
		}
		if attributes.Member == "" || !memberStates[state] {
			return
		}
		if seen, ok := c.peers[attributes.Member]; !ok || record.At.After(seen.At) {
			c.peers[attributes.Member] = PeerState{
				Member: attributes.Member, State: state, At: record.At, SeenBy: record.Source,
			}
		}
		if len(c.reports[attributes.Member]) < maxReports {
			c.reports[attributes.Member] = append(c.reports[attributes.Member],
				stateEvent{at: record.At, state: state, evidence: Reported, seenBy: record.Source})
		} else {
			c.truncated[attributes.Member] = true
		}
		c.unstable.Add(record.At, 0, 0)
	case strings.Contains(message, "state transition"):
		var attributes replAttributes
		record.DecodeAttributes(&attributes)
		if memberStates[attributes.NewState] {
			node.States[attributes.NewState]++
			node.LastState, node.LastStateAt = attributes.NewState, record.At
			// Becoming SECONDARY after a restart is a member joining, and it is as much a part of
			// "the set was not steady" as an election is: a majority write cannot be acknowledged
			// by a node that is still coming up.
			c.unstable.Add(record.At, 0, 0)
			// Entering ROLLBACK is the only report that a node actually rolled back. The word
			// "rollback" also appears in WiredTiger's "rollback to stable" recovery step and in
			// the rollback id a node keeps at startup, neither of which is a replication rollback.
			if attributes.NewState == "ROLLBACK" {
				node.Rollbacks++
			}
			if len(node.Transitions) < maxTransitions {
				node.Transitions = append(node.Transitions, Transition{At: record.At, From: attributes.OldState, To: attributes.NewState})
			} else {
				node.Truncated = true
			}
		}
		if attributes.ReplicaSet != "" {
			node.ReplicaSet = attributes.ReplicaSet
		}
	// Only the node's own "election succeeded" counts a win. "Logging transition to primary to
	// oplog on stepup" is bookkeeping the same node writes afterwards, not a second election.
	case strings.Contains(message, "election succeeded"):
		node.ElectionsWon++
		c.unstable.Add(record.At, 0, 0)
	case strings.Contains(message, "election failed"), strings.Contains(message, "not becoming primary"):
		node.ElectionsLost++
		c.unstable.Add(record.At, 0, 0)
	// A dry run asks whether an election could be won; the real attempt that may follow logs
	// "Starting an election". Counting both reports one election as two.
	case strings.Contains(message, "starting an election"):
		node.ElectionsCalled++
		c.unstable.Add(record.At, 0, 0)
	case strings.Contains(message, "stepping down"), strings.Contains(message, "step down"):
		node.StepDowns++
		c.unstable.Add(record.At, 0, 0)
	case strings.Contains(message, "sync source"):
		node.SyncSourceChange++
	// A node copying a whole data set from a peer is saturating its own disk and its source's, and
	// it explains a slow window on two nodes at once. The phases all say "initial sync"; only the
	// one that begins one is counted, or a single sync would read as dozens.
	case strings.Contains(message, "initial sync"):
		if strings.Contains(message, "starting initial sync") || strings.Contains(message, "initial sync attempt") {
			node.InitialSyncs++
		}
	case strings.Contains(message, "heartbeat"):
		if strings.Contains(message, "fail") || strings.Contains(message, "error") ||
			strings.Contains(message, "timed out") || strings.Contains(message, "retries") {
			node.HeartbeatFailures++
			c.unstable.Add(record.At, 0, 0)
		}
	// Migrations are read before the balancer, because the balancer is what logs most of them and
	// counting those as rounds lost the migration entirely.
	case strings.Contains(message, "movechunk"), strings.Contains(message, "migrat"), strings.Contains(message, "chunk"):
		var attributes replAttributes
		record.DecodeAttributes(&attributes)
		failed := strings.Contains(message, "fail") || strings.Contains(message, "error") || record.Sev == "E"
		if failed {
			node.MigrationErrors++
		} else {
			node.ChunkMigrations++
		}
		if namespace := firstNonEmpty(attributes.Namespace, attributes.NS); namespace != "" {
			c.migration(namespace, record.At, failed)
		}
	case strings.Contains(message, "balancer"):
		node.BalancerRounds++
	}
}

func (c *Cluster) migration(namespace string, at time.Time, failed bool) {
	entry := c.migrations[namespace]
	if entry == nil {
		entry = &Migration{Namespace: namespace, First: at}
		c.migrations[namespace] = entry
	}
	if failed {
		entry.Failures++
	} else {
		entry.Moves++
	}
	if at.Before(entry.First) {
		entry.First = at
	}
	if at.After(entry.Last) {
		entry.Last = at
	}
}

// Migrations returns the namespaces whose chunks moved, the busiest first.
func (c *Cluster) Migrations() []Migration {
	out := make([]Migration, 0, len(c.migrations))
	for _, migration := range c.migrations {
		out = append(out, *migration)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Failures != out[j].Failures {
			return out[i].Failures > out[j].Failures
		}
		if out[i].Moves != out[j].Moves {
			return out[i].Moves > out[j].Moves
		}
		return out[i].Namespace < out[j].Namespace
	})
	return out
}

// Nodes returns the nodes that reported anything, the busiest first.
func (c *Cluster) Nodes() []Node {
	out := make([]Node, 0, len(c.nodes))
	for _, node := range c.nodes {
		// A node that logged something vaguely replication-shaped but nothing that counts is not
		// worth a row of zeros.
		if node.Events() == 0 {
			continue
		}
		sort.Slice(node.Transitions, func(i, j int) bool { return node.Transitions[i].At.Before(node.Transitions[j].At) })
		// Merging runs over a map, so the last state written during the scan is whichever file the
		// range happened to end on. The transitions are ordered by then, and the last of those is
		// the state the node was really left in.
		if len(node.Transitions) > 0 {
			last := node.Transitions[len(node.Transitions)-1]
			node.LastState, node.LastStateAt = last.To, last.At
		}
		out = append(out, *node)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Events() != out[j].Events() {
			return out[i].Events() > out[j].Events()
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// Events is how much happened on a node, which is what decides whether it is worth a look.
func (n Node) Events() int {
	total := n.ElectionsCalled + n.ElectionsWon + n.ElectionsLost + n.StepDowns + n.Rollbacks +
		n.SyncSourceChange + n.MigrationErrors + n.InitialSyncs + n.HeartbeatFailures + n.ChunkMigrations
	for _, count := range n.States {
		total += count
	}
	return total
}

// Trouble is the replication events that are never routine, which is what a screening rule asks
// about: a set that elected, stepped down, rolled back or lost heartbeats did something, and a set
// that only moved chunks did its job.
func (c *Cluster) Trouble() (elections, stepDowns, rollbacks, heartbeats, initialSyncs int) {
	for _, node := range c.nodes {
		elections += node.ElectionsCalled
		stepDowns += node.StepDowns
		rollbacks += node.Rollbacks
		heartbeats += node.HeartbeatFailures
		initialSyncs += node.InitialSyncs
	}
	return elections, stepDowns, rollbacks, heartbeats, initialSyncs
}

// Elections reports whether the set changed primary during the window, which is the question a
// replication summary exists to answer.
func (c *Cluster) Elections() (called, won int) {
	for _, node := range c.nodes {
		called += node.ElectionsCalled
		won += node.ElectionsWon
	}
	return called, won
}
