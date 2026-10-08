package cli

import (
	"fmt"
	"math"
	"mongo-log-parser/internal/funnel"
	"mongo-log-parser/internal/label"
	"os"
	"strings"
	"time"

	"mongo-log-parser/internal/analyze"
	"mongo-log-parser/internal/render"
	"mongo-log-parser/internal/store"
)

// reportSections are the parts of the report, in the order a triage asks for them: what was this,
// what did the labels see, how much was logged over time, what was slow, what did the set do, what
// changed in the catalogue, what did the server complain about, and finally the raw shape of the log.
var reportSections = []string{
	"deployment", "labels", "activity", "slow", "replication", "storage", "ddl", "signals", "logs",
}

// runReport is what the tool does when it is pointed at a path and asked nothing in particular:
// read everything once and show what a person would want to see first.
func runReport(root string, args []string) error {
	fs, c := newFlags("report")
	w := addWindowFlags(fs)
	only := fs.String("only", "", "comma-separated sections to print: "+strings.Join(reportSections, ", "))
	noChart := fs.Bool("no-chart", false, "leave out the log activity chart")
	color := fs.String("color", "auto", "auto, always or never")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	wanted, err := chosenSections(*only)
	if err != nil {
		return err
	}
	started := time.Now()
	st, err := openStore(root, c.refresh)
	if err != nil {
		return err
	}
	defer st.Close()

	// The FTDC streams are opened before anything is read, because a relative window is measured
	// from the last sample and the chunk index knows it without decoding anything.
	set, streamFailures := openReportStreams(st)
	start, end, err := w.resolve(st, set.last)
	if err != nil {
		return err
	}
	q, err := readQuality(st)
	if err != nil {
		return err
	}

	// One pass over the stored lines feeds every log-shaped aggregate at once. A section that
	// needed its own pass would cost the whole bundle again, so everything that reads a log line
	// reads it here.
	groups, queries, cluster := analyze.NewGrouper(), analyze.NewSlowQueries(), analyze.NewCluster()
	deployment, catalogue := analyze.NewDeployment(), analyze.NewDDL()
	health, activity, rebuilds := analyze.NewHealth(), analyze.NewActivity(), analyze.NewRebuilds()
	if err := analyze.Run(st, windowQuery(start, end), groups, queries, cluster, deployment, catalogue, health, activity, rebuilds); err != nil {
		return err
	}
	// A bundle rotated past the last restart has no startup line to read the set name from, and
	// the replication lines still carry it.
	deployment.Learn(cluster)

	states, labelled, cacheSizes, labelFailures := labelStreams(set, start, end)
	streamFailures = append(streamFailures, labelFailures...)
	stall := funnel.Lay(labelled.Results, labelled.Imbalances, labelled.Skipped, openCommand(root))

	shapes := queries.Shapes()
	logGroups := groups.Groups()
	nodes := cluster.Nodes()
	processes := deployment.Processes()

	options := render.TableOptions{Width: outputWidth(c.width), Color: useColor(*color) && !c.json}
	chartFrom, chartTo := slowWindow(start, end, q, shapes)
	bodies := map[string]string{
		"deployment":  deploymentSection(processes, running(set, states, cacheSizes), options),
		"labels":      renderStall(stall, root, options),
		"slow":        renderShapes(shapes, queries, 8, options),
		"replication": renderReplication(nodes, cluster, states, q.FirstAt, q.LastAt, 8, options),
		"storage":     renderStorage(rebuilds, dataSizes(st), 12, options),
		"ddl":         renderCatalogue(catalogue, 6, options),
		"signals":     renderSignals(health.Signals(), 12, options),
		"logs":        renderGroups(logGroups, 10, options),
	}
	if !*noChart {
		if chart := activityChart(activity, chartFrom, chartTo, 0, options.Width, options.Color); chart != "" {
			bodies["activity"] = chart + "\n\n" + severityLine(activity)
		}
	}
	if bodies["activity"] == "" {
		bodies["activity"] = severityLine(activity)
	}

	sections := []string{reportHeading(root, st, q, set, activity, deployment.Summarize())}
	if q.Files == 0 && len(set.streams) == 0 {
		return emit(os.Stdout, c.json, "report", root, map[string]any{"quality": q},
			withVersion(sections[0]+"\n\n"+noLogsNote(st)))
	}
	// A bundle of FTDC alone still answers a great deal: what was running and what the labels saw.
	// Refusing the whole report because no log came with it hides a ticket pool that emptied behind
	// the words "nothing to report on".
	if q.Files == 0 {
		sections = append(sections, noFtdcLogsNote(st))
		for name := range wanted {
			if !metricOnlySections[name] {
				wanted[name] = false
			}
		}
	}
	for _, name := range reportSections {
		if !wanted[name] || bodies[name] == "" {
			continue
		}
		sections = append(sections, section(sectionTitle(name), bodies[name]))
	}

	footer := fmt.Sprintf("%s in %s, %s; %s; drill down with: metrics, slow, logs, lines, ddl, health, deployment, coverage",
		plural(q.Parsed, "log record"), plural(q.Files, "log stream"),
		plural(len(set.streams), "FTDC stream"), time.Since(started).Round(time.Millisecond))
	if len(streamFailures) > 0 {
		footer = strings.Join(streamFailures, "; ") + "\n" + footer
	}
	data := map[string]any{
		"slow_queries": shapes, "log_groups": logGroups,
		"nodes": nodes, "migrations": cluster.Migrations(), "processes": processes,
		"deployment_summary": deployment.Summarize(), "index_builds": catalogue.Builds(),
		"catalogue_changes": catalogue.Changes(), "signals": health.Signals(), "activity": activity,
		"quality": q, "failures": streamFailures,
		"labels": stall, "rules": ruleCatalogue(),
	}
	return emit(os.Stdout, c.json, "report", root, data, withVersion(strings.Join(sections, "\n\n")+"\n\n"+footer))
}

// metricOnlySections are the parts of the report that are drawn from FTDC and need no log line.
// Everything else — the slow queries, the elections, the catalogue changes, the log groups — is
// read out of the log and is simply absent without one.
var metricOnlySections = map[string]bool{"deployment": true, "labels": true}

// noFtdcLogsNote is for a bundle that brought metrics but no log: it says which half of the report
// is missing and why, so the sections that follow are not mistaken for the whole picture.
func noFtdcLogsNote(st *store.Store) string {
	return "no MongoDB log in this bundle: what follows is read from FTDC alone." +
		" The slow queries, replication events, catalogue changes and log groups need a log and are not below."
}

// noLogsNote is the whole answer for a bundle that holds no MongoDB log at all. Printing ten empty
// sections instead says the same thing ten times and buries the one fact that matters: whatever
// this bundle is for, the logs that would answer it are not in it.
func noLogsNote(st *store.Store) string {
	lines := []string{"this bundle holds no MongoDB log that this build can read, so there is nothing to report on."}
	rows, err := st.DB.Query(`SELECT kind, count(*) FROM source GROUP BY kind ORDER BY count(*) DESC`)
	if err == nil {
		defer rows.Close()
		var kinds []string
		for rows.Next() {
			var kind string
			var count int
			if rows.Scan(&kind, &count) == nil {
				kinds = append(kinds, fmt.Sprintf("%s ×%d", kind, count))
			}
		}
		if len(kinds) > 0 {
			lines = append(lines, "", "what it does hold: "+strings.Join(kinds, ", "))
		}
	}
	lines = append(lines, "", "coverage lists every file and why it was or was not read."+
		" A bundle of another database's files, or of shell output alone, is read by neither.")
	return strings.Join(lines, "\n")
}

// deploymentSection is the process table with the version history under it: the table says what is
// running and the history says how it got there, and an upgrade that was rolled back halfway is
// only visible in the second.
func deploymentSection(processes []analyze.Process, fromFTDC []ftdcProcess, options render.TableOptions) string {
	if len(processes) == 0 && len(fromFTDC) > 0 {
		return "no startup lines in this window; what each node's FTDC recorded about itself:\n" +
			renderFTDCProcesses(fromFTDC, options)
	}
	body := renderProcesses(processes, options)
	if history := versionHistory(processes); history != "" {
		body += "\n\n" + history
	}
	return body
}

// sectionTitle is what a section is called on screen; the flag name is the short one a caller types.
func sectionTitle(name string) string {
	switch name {
	case "deployment":
		return "What was running"
	case "activity":
		return "Log activity"
	case "slow":
		return "Slow queries"
	case "replication":
		return "Replication and sharding"
	case "ddl":
		return "Catalogue changes"
	case "signals":
		return "Conditions the server logged"
	case "logs":
		return "Log groups"
	case "storage":
		return "How much data there is"
	case "labels":
		return "What the labels saw"
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// chosenSections turns --only into the set to print, and refuses a name that means nothing rather
// than quietly printing everything.
func chosenSections(only string) (map[string]bool, error) {
	wanted := map[string]bool{}
	if strings.TrimSpace(only) == "" {
		for _, name := range reportSections {
			wanted[name] = true
		}
		return wanted, nil
	}
	known := map[string]bool{}
	for _, name := range reportSections {
		known[name] = true
	}
	for _, name := range strings.Split(only, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if !known[name] {
			return nil, fmt.Errorf("--only %s: no such section; the sections are %s", name, strings.Join(reportSections, ", "))
		}
		wanted[name] = true
	}
	if len(wanted) == 0 {
		return nil, fmt.Errorf("--only named no sections")
	}
	return wanted, nil
}

// openReportStreams opens every FTDC stream in the bundle, reporting rather than refusing when one
// of them cannot be read: a report of the logs is still worth printing when a diagnostic directory
// is truncated.
func openReportStreams(st *store.Store) (*streamSet, []string) {
	set, err := openStreams(st, nil)
	if err != nil {
		return &streamSet{}, []string{err.Error()}
	}
	return set, nil
}

// windowQuery is the stored-line query a window asks for. A zero time means no bound.
func windowQuery(start, end time.Time) store.Query {
	q := store.Query{}
	if !start.IsZero() {
		q.From = start.UnixMilli()
	}
	if !end.IsZero() {
		q.To = end.UnixMilli()
	}
	return q
}

func reportHeading(root string, st *store.Store, q quality, set *streamSet, activity *analyze.Activity, summary analyze.Summary) string {
	var files int
	var bytes int64
	_ = st.DB.QueryRow(`SELECT count(*), coalesce(sum(size), 0) FROM source`).Scan(&files, &bytes)
	read := plural(q.Files, "log stream")
	if skipped := len(q.NotRead); skipped > 0 {
		read = fmt.Sprintf("%s (%d not read)", read, skipped)
	}
	heading := fmt.Sprintf("%s\n%s, %.1f GB | %s | %s\n%s",
		root, plural(files, "file"), float64(bytes)/1e9,
		plural(len(set.streams), "FTDC stream"), read, windowLines(q, activity))
	if line := deploymentLine(summary); line != "" {
		heading += "\n" + line
	}
	return heading
}

// windowLines say what period the report is about, and what the bundle holds around it.
//
// Every figure below is counted over one period, and the reader has to know which. Printing the
// bundle's whole span over counts taken from two hours of it is worse than printing nothing: it
// reads as the whole bundle's worth of evidence. So the first line is always the period the
// numbers came from, and a narrowed one is shown against the bundle's own span underneath.
func windowLines(q quality, activity *analyze.Activity) string {
	if activity == nil || activity.Records == 0 {
		if q.FirstAt.IsZero() {
			return "read     no log records"
		}
		return fmt.Sprintf("read     no log records in this window; the bundle holds %s", period(q.FirstAt, q.LastAt))
	}
	read := fmt.Sprintf("read     %s  ·  %s log records", period(activity.FirstAt, activity.LastAt), thousands(activity.Records))
	// Within a minute of the bundle's own ends, this is the whole bundle and saying so twice is
	// noise; narrower than that, the second line is what stops the first being misread.
	const slack = time.Minute
	whole := !activity.FirstAt.After(q.FirstAt.Add(slack)) && !activity.LastAt.Before(q.LastAt.Add(-slack))
	if whole || q.FirstAt.IsZero() {
		return read
	}
	return read + fmt.Sprintf("\nbundle   %s  ·  the rest is outside the window asked for", period(q.FirstAt, q.LastAt))
}

// askedWindow is the period a command was narrowed to, for the footer of every command that is not
// the report. Empty when none was asked for: a footer that says "the whole bundle" on every run is
// noise, and one that says nothing after --last 2h lets two hours be read as the whole bundle.
func askedWindow(start, end time.Time) string {
	if start.IsZero() && end.IsZero() {
		return ""
	}
	return "window " + period(start, end) + "; "
}

// period is a span a person reads: both ends, and how long it was.
func period(from, to time.Time) string {
	if from.IsZero() {
		return "-"
	}
	right := to.Format("2006-01-02 15:04")
	if from.Format("2006-01-02") == to.Format("2006-01-02") {
		right = to.Format("15:04")
	}
	return fmt.Sprintf("%s → %s UTC (%s)", from.Format("2006-01-02 15:04"), right, duration(to.Sub(from).Milliseconds()))
}

// labelStreams reads each node's FTDC once and labels it, keeping the replica set member states
// that ride along in the same columns.
func labelStreams(set *streamSet, start, end time.Time) ([]replMember, labels, map[string]float64, []string) {
	failures := make([]string, 0)
	cache := map[string]float64{} // the WiredTiger cache each node was last configured with, in bytes
	states := make([]replMember, 0, len(set.streams))
	// The replica set flags are two columns out of thousands; decompressing this data again to read
	// them would cost as much as the labels.
	want := func(path string) bool { return replMemberMetric(path) || label.Wants(path) }
	results, unlabelled := make([]label.Result, 0, 64), make([]label.Skip, 0, 16)
	for i, stream := range set.streams {
		source := set.sources[i]
		samples, err := stream.Read(start, end, want, nil)
		if err != nil {
			failures = append(failures, source+": "+err.Error())
			continue
		}
		// The version FTDC recorded picks each label's definition for this node.
		labelled, skipped := label.Run(samples, source, stream.Identity().Version, start, end)
		results = append(results, labelled...)
		unlabelled = append(unlabelled, skipped...)
		if state, ok := replMemberOf(samples, source); ok {
			states = append(states, state)
		}
		for _, v := range samples.Values[cacheSizeMetric] {
			if !math.IsNaN(v) && v > 0 {
				cache[source] = v
			}
		}
		if line := damageLine(samples, source); line != "" {
			failures = append(failures, line)
		}
	}
	sortMemberStates(states)
	label.Relate(results)
	return states, labels{results, label.Imbalances(results), unlabelled}, cache, failures
}

// cacheSizeMetric is the configured WiredTiger cache, which a label already reads and so is decoded.
const cacheSizeMetric = "serverStatus/wiredTiger/cache/maximum bytes configured"

// labels is what the rules made of the FTDC: every label, how alike the things that should
// be alike were, and the labels a node could not have, with the reason.
type labels struct {
	Results    []label.Result    `json:"results"`
	Imbalances []label.Imbalance `json:"imbalances"`
	Skipped    []label.Skip      `json:"skipped"`
}

// ftdcProcess is what one node's FTDC says it was: FTDC records the server's identity in every
// chunk, so it answers "what was running" when the log never caught a startup.
type ftdcProcess struct {
	Node     string  `json:"node"`
	Process  string  `json:"process"`
	Set      string  `json:"set,omitempty"`
	Version  string  `json:"version"`
	State    string  `json:"state,omitempty"`     // the replica set state it was last recorded in
	CacheGiB float64 `json:"cache_gib,omitempty"` // the WiredTiger cache configured
}

// running reads each stream's identity, beside the state the node last recorded and the cache size
// its labels were read against.
func running(set *streamSet, states []replMember, cache map[string]float64) []ftdcProcess {
	out := make([]ftdcProcess, 0, len(set.streams))
	for i, stream := range set.streams {
		id := stream.Identity()
		p := ftdcProcess{Node: set.sources[i], Process: id.Process, Set: id.SetName, Version: id.Version}
		for _, m := range states {
			if m.Source == p.Node && len(m.Spans) > 0 {
				p.State = m.EndedAs()
			}
		}
		p.CacheGiB = math.Round(cache[p.Node]/(1<<30)*10) / 10
		out = append(out, p)
	}
	return out
}

func renderFTDCProcesses(found []ftdcProcess, options render.TableOptions) string {
	text := func(read func(ftdcProcess) string) []string {
		values := make([]string, len(found))
		for i, p := range found {
			values[i] = orDash(read(p))
		}
		return values
	}
	return render.Table([]render.Column{
		{Title: "Node", Values: text(func(p ftdcProcess) string { return p.Node }), Elastic: true},
		{Title: "Process", Values: text(func(p ftdcProcess) string { return p.Process })},
		{Title: "Set", Values: text(func(p ftdcProcess) string { return p.Set })},
		{Title: "Version", Values: text(func(p ftdcProcess) string { return p.Version })},
		{Title: "Last state", Values: text(func(p ftdcProcess) string { return p.State })},
		{Title: "Cache", Align: render.AlignRight, Values: text(func(p ftdcProcess) string {
			if p.CacheGiB == 0 {
				return ""
			}
			return fmt.Sprintf("%gG", p.CacheGiB)
		})},
	}, options)
}
