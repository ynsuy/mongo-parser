package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"mongo-log-parser/internal/analyze"
	"mongo-log-parser/internal/store"
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

// readEvidence makes one pass over the stored lines of the window and keeps what one node said.
func readEvidence(st *store.Store, root, source string, from, to time.Time) (evidence, error) {
	queries, health, cluster := analyze.NewSlowQueries(), analyze.NewHealth(), analyze.NewCluster()
	catalogue, activity := analyze.NewDDL(), analyze.NewActivity()
	filter := &nodeOnly{source: source, sinks: []analyze.Sink{queries, health, cluster, catalogue, activity}}
	if err := analyze.Run(st, windowQuery(from, to), filter); err != nil {
		return evidence{}, err
	}
	e := evidence{Lines: filter.lines, Elsewhere: filter.others,
		Next: fmt.Sprintf("mongo-log-parser lines %s --node %s --from %s --to %s", root, source,
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
