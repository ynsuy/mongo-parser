package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"mongo-log-parser/internal/analyze"
	"mongo-log-parser/internal/render"
)

// The bodies of the report's sections that have no command of their own to live in. Everything the
// report and a single-section command both draw is in sections.go; this is the rest.

func renderShapes(shapes []analyze.Shape, queries *analyze.SlowQueries, limit int, options render.TableOptions) string {
	if len(shapes) == 0 {
		return "no slow queries in this window"
	}
	shown := shapes[:min(limit, len(shapes))]
	collscanShapes, collscanRuns := queries.Collscans()
	table := render.Table([]render.Column{
		{Title: "Namespace", Values: field(shown, func(s analyze.Shape) string { return s.Namespace })},
		{Title: "Op", Values: field(shown, func(s analyze.Shape) string { return s.Operation })},
		{Title: "Plan", Values: field(shown, func(s analyze.Shape) string { return s.Plan })},
		{Title: "Count", Align: render.AlignRight, Values: field(shown, func(s analyze.Shape) string { return strconv.Itoa(s.Count) })},
		{Title: "Total ms", Align: render.AlignRight, Values: field(shown, func(s analyze.Shape) string { return render.Compact(s.TotalTime()) })},
		{Title: "Avg ms", Align: render.AlignRight, Values: field(shown, func(s analyze.Shape) string { return render.Compact(s.Duration.Avg()) })},
		{Title: "P95 ms", Align: render.AlignRight, Values: field(shown, func(s analyze.Shape) string { return render.Compact(s.Duration.Quantile(0.95)) }), Drop: 3},
		{Title: "Keys", Align: render.AlignRight, Values: field(shown, func(s analyze.Shape) string { return loggedAvg(s.Keys) }), Drop: 2},
		{Title: "Docs", Align: render.AlignRight, Values: field(shown, func(s analyze.Shape) string { return loggedAvg(s.Docs) }), Drop: 4},
		{Title: "Ret", Align: render.AlignRight, Values: field(shown, func(s analyze.Shape) string { return loggedAvg(s.Returned) }), Drop: 3},
		{Title: "Ineff", Align: render.AlignRight, Values: field(shown, inefficiency), Drop: 1},
		{Title: "Outcome", Values: field(shown, func(s analyze.Shape) string { return s.Outcome() })},
		{Title: "Spent on", Values: field(shown, spentOn), Drop: 5},
		{Title: "Query hash", Values: field(shown, func(s analyze.Shape) string { return s.Label() })},
	}, options)
	total, waits := queries.WaitTotals()
	if breakdown := renderWaits(total, waits, options); breakdown != "" {
		table += "\n\nwhere the time went\n" + breakdown
	}
	return fmt.Sprintf("%s\n%s in total (%s), %d of them %s COLLSCAN (%d executions)%s",
		table, plural(len(shapes), "group"), groupingNote(queries.GroupingCounts()),
		collscanShapes, are(collscanShapes), collscanRuns, hashNote(shown))
}

// hashNote explains "not logged" only where a row says it. Printed unconditionally it was a line
// about a word that was not on the screen.
func hashNote(shapes []analyze.Shape) string {
	for _, shape := range shapes {
		if shape.ID == "" {
			return "\n\"not logged\" is a namespace/operation/plan group, not one query shape: the server logged no hash for it."
		}
	}
	return ""
}

// renderReplication is the per-node summary with the per-namespace migrations under it: the nodes
// say what the set did, and the namespaces say which data it did it to.
func renderReplication(nodes []analyze.Node, cluster *analyze.Cluster, states []replMember, from, to time.Time, limit int, options render.TableOptions) string {
	band := renderStateBand(cluster, from, to, options)
	// What the nodes' own diagnostic data says they were. The logs are the better source when they
	// have it — they say why a state changed — but they frequently do not, and this always does.
	fromFTDC := renderMemberStates(states, options)
	if len(nodes) == 0 {
		// A set that held no election and changed no state has nothing for the table above, and
		// what it was doing all that time is still worth seeing: "three members, one primary
		// throughout" is an answer, and an empty section is not.
		switch {
		case band == "" && fromFTDC == "":
			return "no replication or sharding events in this window, and no member logged a state change: nothing here says what any of them was"
		case band == "":
			return "no elections, step downs or migrations in this window, and no member logged a state change\n\n" + fromFTDC
		}
		out := "no elections, step downs or migrations in this window\n\nmember state over the window\n" + band
		if fromFTDC != "" {
			out += "\n\n" + fromFTDC
		}
		return out
	}
	out := renderNodes(nodes, limit, options)
	if band != "" {
		out += "\n\nmember state over the window\n" + band
	}
	if fromFTDC != "" {
		out += "\n\n" + fromFTDC
	}
	if table := renderMigrations(cluster.Migrations(), limit, options); table != "" {
		out += "\n\nchunk migrations by namespace\n" + table
	}
	return out
}

func renderNodes(nodes []analyze.Node, limit int, options render.TableOptions) string {
	shown := nodes[:min(limit, len(nodes))]
	sources := make([]string, len(shown))
	states := make([]string, len(shown))
	for i, node := range shown {
		sources[i] = node.Source
		seen := make([]string, 0, len(node.States))
		for state, count := range node.States {
			seen = append(seen, fmt.Sprintf("%s×%d", state, count))
		}
		sort.Strings(seen)
		states[i] = strings.Join(seen, " ")
		if states[i] == "" {
			states[i] = "-"
		}
	}
	number := func(read func(analyze.Node) int) []string {
		values := make([]string, len(shown))
		for i, node := range shown {
			values[i] = strconv.Itoa(read(node))
		}
		return values
	}
	return render.Table([]render.Column{
		{Title: "Node", Align: render.AlignTail, Values: sources},
		// Drop order is what survives a narrow terminal, so it is the order these matter in. A set
		// that stepped down dozens of times and a set that rolled back are the two things this section
		// exists to report; sync source changes and chunk counts go first.
		{Title: "Ended as", Values: field2(shown, func(n analyze.Node) string { return orDash(n.LastState) }), Drop: 7},
		{Title: "States seen", Values: states, Elastic: true},
		{Title: "Elections", Align: render.AlignRight, Values: number(func(n analyze.Node) int { return n.ElectionsCalled })},
		{Title: "Won", Align: render.AlignRight, Values: number(func(n analyze.Node) int { return n.ElectionsWon })},
		{Title: "Stepdowns", Align: render.AlignRight, Values: number(func(n analyze.Node) int { return n.StepDowns })},
		{Title: "Rollbacks", Align: render.AlignRight, Values: number(func(n analyze.Node) int { return n.Rollbacks })},
		{Title: "HB fail", Align: render.AlignRight, Values: number(func(n analyze.Node) int { return n.HeartbeatFailures }), Drop: 6},
		{Title: "Init sync", Align: render.AlignRight, Values: number(func(n analyze.Node) int { return n.InitialSyncs }), Drop: 5},
		{Title: "Migr err", Align: render.AlignRight, Values: number(func(n analyze.Node) int { return n.MigrationErrors }), Drop: 4},
		{Title: "Chunks", Align: render.AlignRight, Values: number(func(n analyze.Node) int { return n.ChunkMigrations }), Drop: 2},
		{Title: "Sync src", Align: render.AlignRight, Values: number(func(n analyze.Node) int { return n.SyncSourceChange }), Drop: 1},
	}, options)
}

// renderCatalogue is the index builds and the catalogue changes together, which is how they are
// read: a drop at 02:00 and a build that started at 02:01 are one story.
func renderCatalogue(catalogue *analyze.DDL, limit int, options render.TableOptions) string {
	builds, changes := catalogue.Builds(), catalogue.Changes()
	if len(builds) == 0 && len(changes) == 0 {
		return "no catalogue changes in this window"
	}
	parts := make([]string, 0, 2)
	if len(builds) > 0 {
		parts = append(parts, renderBuilds(builds, limit, options))
	}
	if len(changes) > 0 {
		parts = append(parts, renderChanges(changes, limit, options))
	}
	return strings.Join(parts, "\n\n")
}

func renderGroups(groups []analyze.Group, limit int, options render.TableOptions) string {
	if len(groups) == 0 {
		return "no log lines in this window"
	}
	shown := groups[:min(limit, len(groups))]
	text := func(read func(analyze.Group) string) []string {
		values := make([]string, len(shown))
		for i, group := range shown {
			values[i] = read(group)
		}
		return values
	}
	return render.Table([]render.Column{
		{Title: "Group", Values: text(func(g analyze.Group) string { return g.ID }), Drop: 4},
		{Title: "Sev", Values: text(func(g analyze.Group) string { return g.Severity })},
		{Title: "Component", Values: text(func(g analyze.Group) string { return g.Component }), Drop: 3},
		{Title: "Count", Align: render.AlignRight, Values: text(func(g analyze.Group) string { return strconv.Itoa(g.Count) })},
		{Title: "Category", Values: text(func(g analyze.Group) string { return string(g.Category) }), Drop: 1},
		{Title: "Message", Values: text(func(g analyze.Group) string { return g.Message })},
	}, options)
}

// field2 is field for nodes; the shapes already have one and Go has no generic over both here.
func field2(nodes []analyze.Node, read func(analyze.Node) string) []string {
	values := make([]string, len(nodes))
	for i, node := range nodes {
		values[i] = read(node)
	}
	return values
}
