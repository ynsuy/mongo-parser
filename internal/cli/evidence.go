package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ynsuy/mongo-parser/internal/analyze"
	"github.com/ynsuy/mongo-parser/internal/store"
)

// evidence is what one node's log said in the seconds a label was opened on. It is the log half of
// the drill-down: the labels say what the server's numbers did, and this says what the server wrote
// about it at the same time. It counts and names; the lines themselves are one `lines` away.
type evidence struct {
	Lines    int `json:"lines"`
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	// Slow is the operations logged as slow that finished in the window, and the shapes that cost
	// the most of their time.
	Slow       int        `json:"slow"`
	SlowMillis float64    `json:"slow_millis"`
	SlowShapes []slowLine `json:"slow_shapes,omitempty"`
	// Waited is where the slow operations' time went, when the log said.
	Waited []analyze.Wait `json:"waited,omitempty"`
	// Conditions are the named conditions the server logged, with how often.
	Conditions []string `json:"conditions,omitempty"`
	// Replication is the elections, step downs, rollbacks and lost heartbeats in the window.
	Replication []string `json:"replication,omitempty"`
	// IndexBuilds are the builds running in the window.
	IndexBuilds []string `json:"index_builds,omitempty"`
	// Elsewhere is how many lines the other nodes logged in the same window, so a quiet node in a
	// loud cluster is not read as a quiet cluster.
	Elsewhere int `json:"elsewhere"`
	// Lines is the command that prints this node's lines for the window.
	Next string `json:"next"`
	// Matched says how the log was tied to the node when the names did not agree exactly, and is
	// empty when they did. A log matched by its short name alone may be another node's.
	Matched string `json:"matched,omitempty"`
}

type slowLine struct {
	Namespace string  `json:"ns"`
	Operation string  `json:"operation"`
	Plan      string  `json:"plan"`
	Count     int     `json:"count"`
	Millis    float64 `json:"millis"`
	Hash      string  `json:"hash"`
}

// nodeOnly passes on the lines one node wrote, and counts the rest.
type nodeOnly struct {
	source string
	sinks  []analyze.Sink
	lines  int
	others int
}

func (n *nodeOnly) Add(r analyze.Record) {
	if r.Source != n.source {
		n.others++
		return
	}
	n.lines++
	for _, sink := range n.sinks {
		sink.Add(r)
	}
}

// logSource is the name the node's log goes by, for a node read from FTDC. They agree when the
// bundle tied the two together. When it could not — a log export that names its pod and nothing
// more, beside FTDC that names the full host — the log is taken by its short host name, and the
// note says so, and says when other nodes share that short name and the log may be theirs.
func logSource(st *store.Store, source string, ftdcSources []string) (string, string) {
	labels, err := analyze.Labels(st)
	if err != nil {
		return source, ""
	}
	short := func(name string) string {
		name, _, _ = strings.Cut(name, "@")
		name, _, _ = strings.Cut(name, ":")
		return strings.ToLower(name)
	}
	candidates := map[string]bool{}
	for _, label := range labels {
		if label == source {
			return source, ""
		}
		if short(label) == short(source) {
			candidates[label] = true
		}
	}
	if len(candidates) != 1 {
		return source, ""
	}
	var found string
	for label := range candidates {
		found = label
	}
	sharing := 0
	for _, other := range ftdcSources {
		if short(other) == short(source) {
			sharing++
		}
	}
	note := fmt.Sprintf("the log names this node only %s; it was matched by that short name", found)
	if sharing > 1 {
		note += fmt.Sprintf(", which %d nodes here share, so these lines may be another of them", sharing)
	}
	return found, note
}

// readEvidence makes one pass over the stored lines of the window and keeps what one node said.
func readEvidence(st *store.Store, root, source string, ftdcSources []string, from, to time.Time) (evidence, error) {
	logName, matched := logSource(st, source, ftdcSources)
	queries, health, cluster := analyze.NewSlowQueries(), analyze.NewHealth(), analyze.NewCluster()
	catalogue, activity := analyze.NewDDL(), analyze.NewActivity()
	filter := &nodeOnly{source: logName, sinks: []analyze.Sink{queries, health, cluster, catalogue, activity}}
	if err := analyze.Run(st, windowQuery(from, to), filter); err != nil {
		return evidence{}, err
	}
	e := evidence{Lines: filter.lines, Elsewhere: filter.others, Matched: matched,
		Next: fmt.Sprintf("mongo-parser lines %s --node %s --from %s --to %s", root, logName,
			from.Format("2006-01-02T15:04:05Z"), to.Format("2006-01-02T15:04:05Z"))}
	e.Errors, e.Warnings = activity.Problems()

	shapes := queries.Shapes()
	for _, s := range shapes {
		e.Slow += s.Count
	}
	e.SlowMillis = queries.TotalTime()
	sort.SliceStable(shapes, func(i, j int) bool { return shapes[i].TotalTime() > shapes[j].TotalTime() })
	for _, s := range shapes[:min(3, len(shapes))] {
		e.SlowShapes = append(e.SlowShapes, slowLine{Namespace: s.Namespace, Operation: s.Operation, Plan: s.Plan,
			Count: s.Count, Millis: s.TotalTime(), Hash: s.Label()})
	}
	if _, waits := queries.WaitTotals(); len(waits) > 0 {
		e.Waited = waits[:min(3, len(waits))]
	}
	for _, signal := range health.Signals() {
		e.Conditions = append(e.Conditions, fmt.Sprintf("%s ×%d", signal.Title, signal.Count))
	}
	elections, stepDowns, rollbacks, heartbeats, initialSyncs := cluster.Trouble()
	for _, event := range []struct {
		n    int
		name string
	}{{elections, "election"}, {stepDowns, "step down"}, {rollbacks, "rollback"}, {heartbeats, "heartbeat failure"}, {initialSyncs, "initial sync"}} {
		if event.n > 0 {
			e.Replication = append(e.Replication, plural(event.n, event.name))
		}
	}
	for _, b := range catalogue.Builds() {
		if b.Started.After(to) || (!b.Finished.IsZero() && b.Finished.Before(from)) {
			continue
		}
		e.IndexBuilds = append(e.IndexBuilds, strings.TrimSpace(b.Namespace+" "+b.Indexes)+" ("+b.Outcome+")")
	}
	return e, nil
}
