package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"mongo-log-parser/internal/analyze"
	"mongo-log-parser/internal/metrics"
	"mongo-log-parser/internal/render"
	"mongo-log-parser/internal/series"
)

// The sections here are shared between the overall report and the commands that show one of them in
// full. A section that looked different depending on which command drew it would read as two
// different answers to the same question.

func section(title, body string) string {
	return "── " + title + " " + strings.Repeat("─", max(0, 60-len(title))) + "\n" + body
}

// renderProcesses says what was running: the line a triage needs before it reads any other number,
// and the one a bundle almost never states anywhere a person can find it.
//
// There is no column for the log files any more. The store settles at ingest which rotated files
// are one node and names the node for them, so Source is already the answer a reader wants; which
// files it was read from is a question about the bundle, and `coverage` answers that one.
func renderProcesses(processes []analyze.Process, options render.TableOptions) string {
	if len(processes) == 0 {
		return "no startup lines in this window, so version, cache size and cluster role cannot be read: they are only ever logged at startup"
	}
	text := func(read func(analyze.Process) string) []string {
		values := make([]string, len(processes))
		for i, process := range processes {
			values[i] = read(process)
		}
		return values
	}
	return render.Table([]render.Column{
		// The node's own name for itself, which is what its replica set config and rs.status call
		// it — and what tells a mongos on :27018 from the config server on :27019 of the same
		// machine, which two log file names beside each other do not.
		{Title: "Node", Align: render.AlignTail, Values: text(func(p analyze.Process) string { return p.Source })},
		{Title: "Role", Values: text(func(p analyze.Process) string { return p.Role })},
		{Title: "Set", Values: text(func(p analyze.Process) string { return orDash(p.ReplicaSet) })},
		{Title: "Version", Values: text(func(p analyze.Process) string { return orDash(p.Version) })},
		// The version a process is running and the versions it ran are different facts, and joining
		// them into one cell answered neither: a node reading "5.0.0, 6.0.0" does not say which
		// of the two is live now.
		{Title: "Was", Values: text(func(p analyze.Process) string { return orDash(strings.Join(p.Previous(), ", ")) })},
		{Title: "Last start", Values: text(func(p analyze.Process) string {
			if p.LastStartAt.IsZero() {
				return "-"
			}
			return p.LastStartAt.Format("01-02 15:04")
		})},
		{Title: "Engine", Values: text(func(p analyze.Process) string { return orDash(p.StorageEngine) })},
		{Title: "Cache", Align: render.AlignRight, Values: text(func(p analyze.Process) string {
			if p.CacheGB == 0 {
				return "-"
			}
			return render.Compact(p.CacheGB) + "G"
		})},
		{Title: "Starts", Align: render.AlignRight, Values: text(func(p analyze.Process) string { return strconv.Itoa(p.Starts) })},
		{Title: "Unclean", Align: render.AlignRight, Values: text(func(p analyze.Process) string { return strconv.Itoa(p.Unclean) })},
		{Title: "Auth", Values: text(func(p analyze.Process) string { return orDash(p.Authorization) })},
		{Title: "OS", Values: text(func(p analyze.Process) string { return orDash(p.OS) })},
	}, options)
}

// deploymentLine is the whole deployment in one sentence, for the top of a report.
func deploymentLine(summary analyze.Summary) string {
	if summary.Processes == 0 {
		return ""
	}
	parts := []string{strings.Join(summary.Roles, ", ")}
	if len(summary.Versions) > 0 {
		parts = append(parts, "MongoDB "+strings.Join(summary.Versions, "/"))
	}
	if len(summary.Engines) > 0 {
		parts = append(parts, strings.Join(summary.Engines, "/"))
	}
	if len(summary.ReplicaSets) > 0 {
		parts = append(parts, plural(len(summary.ReplicaSets), "replica set")+" ("+strings.Join(summary.ReplicaSets, ", ")+")")
	}
	if summary.Sharded {
		parts = append(parts, "sharded")
	}
	if summary.Restarts > 0 {
		parts = append(parts, plural(summary.Restarts, "restart")+" in window")
	}
	if summary.Unclean > 0 {
		parts = append(parts, plural(summary.Unclean, "unclean shutdown"))
	}
	return strings.Join(parts, " | ")
}

// renderBuilds shows the index builds, which are the catalogue changes with a duration worth
// reading: everything else is instantaneous and only its timing matters.
func renderBuilds(builds []analyze.IndexBuild, limit int, options render.TableOptions) string {
	if len(builds) == 0 {
		return "no index builds in this window"
	}
	shown := builds[:min(limit, len(builds))]
	text := func(read func(analyze.IndexBuild) string) []string {
		values := make([]string, len(shown))
		for i, build := range shown {
			values[i] = read(build)
		}
		return values
	}
	table := render.Table([]render.Column{
		{Title: "Namespace", Values: text(func(b analyze.IndexBuild) string { return b.Namespace })},
		{Title: "Index", Values: text(func(b analyze.IndexBuild) string { return orDash(b.Indexes) })},
		{Title: "Started", Values: text(func(b analyze.IndexBuild) string { return b.Started.Format("01-02 15:04") })},
		{Title: "Took", Align: render.AlignRight, Values: text(func(b analyze.IndexBuild) string { return render.Duration(b.Duration()) })},
		{Title: "Outcome", Values: text(func(b analyze.IndexBuild) string { return b.Outcome })},
		{Title: "Phases", Align: render.AlignRight, Values: text(func(b analyze.IndexBuild) string { return strconv.Itoa(b.Phases) })},
		{Title: "Node", Align: render.AlignTail, Values: text(func(b analyze.IndexBuild) string { return b.Source })},
	}, options)
	if len(builds) > len(shown) {
		table += fmt.Sprintf("\n%s in total", plural(len(builds), "build"))
	}
	// Said only when one of them is, because a note explaining a word that is not on the screen is
	// a line the reader has to read to discover it did not apply to them.
	for _, build := range shown {
		if build.Outcome == "running" {
			return table + "\n\"running\" means no completion was logged inside this window; the build may have finished in a file the bundle does not carry."
		}
	}
	return table
}

// renderChanges shows the catalogue changes that have no duration: drops, creates, renames.
func renderChanges(changes []analyze.Change, limit int, options render.TableOptions) string {
	if len(changes) == 0 {
		return "no catalogue changes in this window"
	}
	shown := changes[:min(limit, len(changes))]
	text := func(read func(analyze.Change) string) []string {
		values := make([]string, len(shown))
		for i, change := range shown {
			values[i] = read(change)
		}
		return values
	}
	table := render.Table([]render.Column{
		{Title: "Change", Values: text(func(c analyze.Change) string { return c.Kind })},
		{Title: "Namespace", Values: text(func(c analyze.Change) string { return orDash(c.Namespace) })},
		{Title: "Count", Align: render.AlignRight, Values: text(func(c analyze.Change) string { return strconv.Itoa(c.Count) })},
		{Title: "Failed", Align: render.AlignRight, Values: text(func(c analyze.Change) string { return strconv.Itoa(c.Failures) })},
		{Title: "First", Values: text(func(c analyze.Change) string { return c.First.Format("01-02 15:04") })},
		{Title: "Last", Values: text(func(c analyze.Change) string { return c.Last.Format("01-02 15:04") })},
		{Title: "Slowest ms", Align: render.AlignRight, Values: text(func(c analyze.Change) string {
			if c.MaxMillis == 0 {
				return "-"
			}
			return render.Compact(c.MaxMillis)
		})},
	}, options)
	if len(changes) > len(shown) {
		table += fmt.Sprintf("\n%d change groups in total", len(changes))
	}
	return table
}

// renderSignals shows the conditions the server logged without ever raising a severity for them.
func renderSignals(signals []analyze.Signal, limit int, options render.TableOptions) string {
	if len(signals) == 0 {
		return "no configured log-message patterns matched in this window"
	}
	shown := signals[:min(limit, len(signals))]
	text := func(read func(analyze.Signal) string) []string {
		values := make([]string, len(shown))
		for i, signal := range shown {
			values[i] = read(signal)
		}
		return values
	}
	table := render.Table([]render.Column{
		{Title: "Review", Values: text(func(s analyze.Signal) string { return s.Priority })},
		{Title: "Condition", Values: text(func(s analyze.Signal) string { return s.Title })},
		{Title: "Count", Align: render.AlignRight, Values: text(func(s analyze.Signal) string { return strconv.Itoa(s.Count) })},
		{Title: "Per min*", Align: render.AlignRight, Values: text(func(s analyze.Signal) string { return render.Compact(s.Rate()) })},
		{Title: "First", Values: text(func(s analyze.Signal) string { return s.First.Format("01-02 15:04") })},
		{Title: "Last", Values: text(func(s analyze.Signal) string { return s.Last.Format("01-02 15:04") })},
		{Title: "Nodes", Align: render.AlignRight, Values: text(func(s analyze.Signal) string { return strconv.Itoa(len(s.Sources)) })},
	}, options)
	table += "\n* Rate uses the first-to-last matched-record span, with a minimum denominator of one minute; it is not a full-coverage rate."
	return table
}

// renderMigrations names the collections whose chunks moved, which per-node counters cannot.
func renderMigrations(migrations []analyze.Migration, limit int, options render.TableOptions) string {
	if len(migrations) == 0 {
		return ""
	}
	shown := migrations[:min(limit, len(migrations))]
	text := func(read func(analyze.Migration) string) []string {
		values := make([]string, len(shown))
		for i, migration := range shown {
			values[i] = read(migration)
		}
		return values
	}
	return render.Table([]render.Column{
		{Title: "Namespace", Values: text(func(m analyze.Migration) string { return m.Namespace })},
		{Title: "Moves", Align: render.AlignRight, Values: text(func(m analyze.Migration) string { return strconv.Itoa(m.Moves) })},
		{Title: "Failed", Align: render.AlignRight, Values: text(func(m analyze.Migration) string { return strconv.Itoa(m.Failures) })},
		{Title: "First", Values: text(func(m analyze.Migration) string { return m.First.Format("01-02 15:04") })},
		{Title: "Last", Values: text(func(m analyze.Migration) string { return m.Last.Format("01-02 15:04") })},
	}, options)
}

// activityChart draws how much was logged over time, split by how bad it was.
//
// Every other section of a report is a total over the window, and a total cannot tell a steady
// background from an incident. The lines are severities rather than components because that is the
// split that decides whether a spike is worth chasing: a thousand extra lines a minute of ordinary
// connection notes is a busy hour, and fifty errors a minute is the thing that happened.
func activityChart(activity *analyze.Activity, first, last time.Time, binSeconds int64, width int, color bool) string {
	if activity == nil || len(activity.All) == 0 || !last.After(first) {
		return ""
	}
	slots := render.BarColumns(width)
	if binSeconds <= 0 {
		binSeconds = metrics.AutoBucketSecondsFor(last.Sub(first), analyze.TimelineStep, slots)
	}
	from, to := float64(alignDown(first, binSeconds).UnixMilli()), float64(alignDown(last, binSeconds).UnixMilli())
	errors, warnings := activity.Problems()
	panels := make([]string, 0, 2)

	// Trouble gets its own panel. On one axis with the total it was a flat line on the floor: a
	// deployment writing four hundred lines a minute and two errors in the whole window draws the
	// two errors at zero, which is the opposite of what the chart was added to show.
	if errors > 0 || warnings > 0 {
		lines := make([]series.Series, 0, 2)
		if errors > 0 {
			lines = append(lines, countSeries("errors (E/F)", fold(activity.Errors, binSeconds), first, last, binSeconds))
		}
		if warnings > 0 {
			lines = append(lines, countSeries("warnings (W)", fold(activity.Warnings, binSeconds), first, last, binSeconds))
		}
		panels = append(panels, barPanel(lines, render.BarOptions{
			Width: width, Height: slowChartRows, First: from, Last: to,
			Title: fmt.Sprintf("Errors and warnings per %s  ·  %s and %s in the window",
				render.BinLabel(binSeconds), thousands(errors), thousands(warnings)),
		}))
	}

	// The volume panel says what the deployment was doing, split by what the lines were about when
	// nothing was wrong enough to have a severity.
	lines := []series.Series{countSeries("all lines", fold(activity.All, binSeconds), first, last, binSeconds)}
	if errors == 0 && warnings == 0 {
		lines = nil
		for _, category := range activity.TopCategories(3) {
			lines = append(lines, countSeries(string(category), fold(activity.Category(category), binSeconds), first, last, binSeconds))
		}
	}
	panels = append(panels, barPanel(lines, render.BarOptions{
		Width: width, Height: slowChartRows, First: from, Last: to,
		Title: "Log lines per " + render.BinLabel(binSeconds),
	}))

	return strings.Join(panels, "\n")
}

func fold(timeline analyze.Timeline, binSeconds int64) analyze.Timeline {
	folded := analyze.Timeline{}
	timeline.Fold(binSeconds, folded)
	return folded
}

// severityLine is the whole log in one sentence: how many lines, and how many the server itself
// considered a problem.
func severityLine(activity *analyze.Activity) string {
	if activity == nil || len(activity.Severities) == 0 {
		return ""
	}
	letters := make([]string, 0, len(activity.Severities))
	for letter := range activity.Severities {
		letters = append(letters, letter)
	}
	sort.Slice(letters, func(i, j int) bool { return severityOf(letters[i]) < severityOf(letters[j]) })
	parts := make([]string, 0, len(letters))
	for _, letter := range letters {
		parts = append(parts, fmt.Sprintf("%s×%d", orDash(letter), activity.Severities[letter]))
	}
	categories := make([]string, 0, 4)
	for _, category := range activity.TopCategories(4) {
		categories = append(categories, fmt.Sprintf("%s×%d", category, activity.Categories[category]))
	}
	return "severities: " + strings.Join(parts, " ") + "\nmostly: " + strings.Join(categories, " ")
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// renderWaits says where the time went, which is the question a table of durations cannot answer.
//
// A five-second insert on a machine with CPU and disk to spare has not spent five seconds doing
// anything; it has spent them waiting, and the log line says what for. Every other view in this
// tool ranks operations by how long they took, which puts the query that waited on flow control
// next to the one that scanned a collection and calls them the same problem.
func renderWaits(total float64, waits []analyze.Wait, options render.TableOptions) string {
	if len(waits) == 0 || total <= 0 {
		return ""
	}
	// With nothing but "unaccounted" and a rounding error of storage reads, the breakdown has no
	// finding in it and printing it says "we measured, and it was work" in fifteen lines. The
	// server either reported the waits or it did not.
	accounted := 0.0
	for _, wait := range waits {
		if wait.Name != "unaccounted" && wait.Name != "cpu" {
			accounted += wait.Share
		}
	}
	if accounted < minAccountedShare {
		return ""
	}
	names := make([]string, len(waits))
	millis := make([]string, len(waits))
	shares := make([]string, len(waits))
	for i, wait := range waits {
		names[i] = wait.Name
		millis[i] = render.Compact(wait.Millis)
		shares[i] = fmt.Sprintf("%.1f%%", wait.Share*100)
	}
	table := render.Table([]render.Column{
		{Title: "Spent on", Values: names},
		{Title: "Total ms", Align: render.AlignRight, Values: millis},
		{Title: "Share", Align: render.AlignRight, Values: shares},
	}, options)
	lead := waits[0]
	return fmt.Sprintf("%s\n%s ms in total; the largest share is %s — %s.", table, render.Compact(total), lead.Name, lead.Meaning)
}

// minAccountedShare is how much of the time the server has to have explained before a breakdown of
// it is worth a table.
const minAccountedShare = 0.05

// loggedAvg is a measure's mean over the executions whose line carried it, or "-" when none did.
func loggedAvg(n analyze.Numbers) string {
	if n.Count == 0 {
		return "-"
	}
	return render.Compact(n.Avg())
}

// inefficiency is documents examined per document returned, for a column: "-" for a write, whose
// nreturned means nothing, and for a shape no line of which carried the two counts.
func inefficiency(s analyze.Shape) string {
	if !s.Reads() || s.Docs.Count == 0 || s.Returned.Count == 0 {
		return "-"
	}
	return render.Compact(s.Inefficiency())
}

// spentOn is the two largest things a shape's time went on, each with its share of the duration,
// for a column. The unaccounted remainder is named like the rest: a shape that spent nine tenths of
// its time on nothing the server measured is not one that mostly waited for its storage reads.
func spentOn(shape analyze.Shape) string {
	parts := make([]string, 0, 2)
	for _, wait := range shape.Waits() {
		if wait.Share < 0.005 { // rounds to 0%, and a column of "cpu 0%" says nothing
			break
		}
		parts = append(parts, fmt.Sprintf("%s %.0f%%", wait.Name, wait.Share*100))
		if len(parts) == 2 {
			break
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

// dominantWait is the one thing a shape mostly waited for, short enough for a table column. Work is
// not a wait, so a shape that spent its time on the CPU says so rather than naming a runner-up.
func dominantWait(shape analyze.Shape) string {
	waits := shape.Waits()
	if len(waits) == 0 {
		return "-"
	}
	top := waits[0]
	if top.Name == "cpu" || top.Name == "unaccounted" {
		return "-"
	}
	return fmt.Sprintf("%s %.0f%%", top.Name, top.Share*100)
}

// versionHistory is which binary each process ran, and from when.
//
// A set of versions in one cell says both were seen. In order, with the times, it says whether the
// upgrade finished, stalled halfway, or was rolled back — and those are three different support
// cases that a set cannot tell apart.
func versionHistory(processes []analyze.Process) string {
	// Only when something changed. A list of one version per process repeats the Version column
	// above it, line for line, and says nothing the column did not.
	changed := false
	for _, process := range processes {
		if len(process.VersionChanges()) > 1 {
			changed = true
		}
	}
	if !changed {
		return ""
	}
	lines := make([]string, 0, len(processes))
	rolled := false
	for _, process := range processes {
		changes := process.VersionChanges()
		if len(changes) == 0 {
			continue
		}
		steps := make([]string, 0, len(changes))
		for _, change := range changes {
			steps = append(steps, change.At.Format("01-02 15:04")+" "+change.Version)
		}
		line := fmt.Sprintf("%-40s %s", process.Source, strings.Join(steps, "  →  "))
		if process.Downgraded() {
			line += "   ← went back to a version it had left"
			rolled = true
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	out := "version at each startup\n" + strings.Join(lines, "\n")
	if rolled {
		out += "\nRead off the order alone: a version reappearing after a different one went back. No version numbers are compared."
	}
	return out
}

// bandKeys is how each replica set state is drawn. The letters are the states' own initials where
// they can be — a reader who knows MongoDB reads P and S without a legend — and the two startup
// states share a shape because what matters about both is that the member was not serving yet.
var bandKeys = []render.BandKey{
	{State: "PRIMARY", Glyph: 'P', Legend: "PRIMARY"},
	{State: "SECONDARY", Glyph: 'S', Legend: "SECONDARY"},
	{State: "RECOVERING", Glyph: 'r', Legend: "RECOVERING"},
	// The two startup states share a glyph and a name: what matters about both is that the member
	// was not serving yet, and two legend entries for one character reads as a mistake.
	{State: "STARTUP", Glyph: '^', Legend: "starting up"},
	{State: "STARTUP2", Glyph: '^', Legend: ""},
	{State: "ROLLBACK", Glyph: 'B', Legend: "ROLLBACK"},
	{State: "ARBITER", Glyph: 'A', Legend: "ARBITER"},
	{State: "REMOVED", Glyph: 'x', Legend: "REMOVED"},
	// Not one of MongoDB's states: the process had exited. Drawn as an obvious absence, because a
	// set that is switched off and a set that cannot elect anybody look identical in every count.
	{State: analyze.StateStopped, Glyph: '_', Legend: "shut down"},
	// Only a peer ever reports this state, and it means that peer could not reach the member — not
	// that the member said anything about itself. Naming it "DOWN" invited the stronger reading.
	{State: "DOWN", Glyph: '!', Legend: "unreachable from its peers"},
	{State: "UNKNOWN", Glyph: '?', Legend: "state unknown to its peer"},
}

// renderStateBand draws what every member of every replica set was doing, moment by moment.
//
// The per-node table above it is a set of totals, and totals cannot answer the question a
// replication problem is actually about: was there somebody to write to, and when was there not.
// Nor can they be read across nodes — each row's "last state" is that node's own last state, at
// whatever hour its log happened to stop, and reading them together as a snapshot of one moment can
// report a set that had a primary throughout as having none.
func renderStateBand(cluster *analyze.Cluster, from, to time.Time, options render.TableOptions) string {
	if !to.After(from) {
		return ""
	}
	members := cluster.Members(from, to)
	if len(members) == 0 {
		return ""
	}
	// A set whose members never logged a state change and were never reported by a peer has a band
	// of nothing but gaps. Saying so is shorter and truer than drawing eighty dots per member: a
	// steady set logs a transition when something happens, and nothing did.
	silent := true
	for _, member := range members {
		if member.Known() {
			silent = false
			break
		}
	}
	if silent {
		return ""
	}
	writable := cluster.Writable(from, to, members)

	groups := make([]render.BandGroup, 0, 4)
	for _, set := range setsOf(members) {
		group := render.BandGroup{Title: setTitle(set, set)}
		if spans := writable[set]; len(spans) > 0 {
			lead := render.BandRow{Label: "writable", Spans: bandSpans(spans)}
			group.Lead = &lead
		}
		for _, member := range members {
			if member.Set != set {
				continue
			}
			label := memberLabel(member)
			if member.Partial {
				// The band below this row is drawn from the changes that were kept, and there were
				// more. Marked rather than drawn as if it were the whole history.
				label += " ~"
			}
			group.Rows = append(group.Rows, render.BandRow{
				Label: label,
				Spans: bandSpans(member.Spans),
				// A member with no log of its own is drawn dimmed: every glyph on that row is a
				// peer's account of it, not the member's own word.
				Muted: !member.HasLog(),
			})
		}
		groups = append(groups, group)
	}
	band := render.Band(groups, render.BandOptions{
		Width: options.Width, Color: options.Color, From: from, To: to, Keys: bandKeys,
		Unknown: render.BandKey{State: "", Glyph: '·', Legend: "no log covering it"},
		Mixed:   render.BandKey{State: "", Glyph: '×', Legend: "no one state held that column"},
	})
	if table := setSummaries(cluster.Summaries(from, to, members), options); table != "" {
		band += "\n" + table
	}
	return band
}

// setSummaries states in a table what the band can only draw.
//
// A column of the chart above is a quarter of an hour or more, so a set that got a primary back
// a few minutes before its logs ended shows nothing at all — the column it happened in was mostly
// something else, and drawing it as PRIMARY would be a worse lie than leaving it out. The moments
// themselves are exact and belong in figures.
func setSummaries(summaries []analyze.SetSummary, options render.TableOptions) string {
	if len(summaries) == 0 {
		return ""
	}
	text := func(read func(analyze.SetSummary) string) []string {
		values := make([]string, len(summaries))
		for i, summary := range summaries {
			values[i] = read(summary)
		}
		return values
	}
	return render.Table([]render.Column{
		{Title: "Set", Values: text(func(summary analyze.SetSummary) string {
			return setTitle(summary.Set, summary.Set)
		})},
		{Title: "Members", Align: render.AlignRight, Values: text(func(summary analyze.SetSummary) string {
			if summary.WithLogs == summary.Members {
				return strconv.Itoa(summary.Members)
			}
			// A member with no log here is one the bundle only heard about, and a set reported on
			// from a minority of its members' logs is a set nothing in this report can speak for.
			return fmt.Sprintf("%d of %d", summary.WithLogs, summary.Members)
		})},
		{Title: "No PRIMARY", Align: render.AlignRight, Values: text(func(summary analyze.SetSummary) string {
			return render.Duration(summary.LongestGap)
		})},
		{Title: "From", Values: text(func(summary analyze.SetSummary) string {
			if summary.GapStarted.IsZero() {
				return "-"
			}
			return summary.GapStarted.Format("01-02 15:04")
		})},
		{Title: "Total", Align: render.AlignRight, Values: text(func(summary analyze.SetSummary) string {
			return render.Duration(summary.TotalGap)
		})},
		{Title: "Last in PRIMARY", Align: render.AlignTail, Values: text(func(summary analyze.SetSummary) string {
			if summary.LastPrimary == "" {
				return "never seen"
			}
			return shortHost(summary.LastPrimary)
		})},
		{Title: "At", Values: text(func(summary analyze.SetSummary) string {
			if summary.LastPrimaryAt.IsZero() {
				return "-"
			}
			return summary.LastPrimaryAt.Format("01-02 15:04")
		})},
		{Title: "Knows how", Values: text(func(summary analyze.SetSummary) string {
			// Whether the last word on the set came from the member itself or from somebody
			// watching it decides how much weight to put on it.
			if summary.LastPrimaryBy == "" {
				return "-"
			}
			return string(summary.LastPrimaryBy)
		})},
		{Title: "Shut down", Align: render.AlignRight, Values: text(func(summary analyze.SetSummary) string {
			// Beside "No PRIMARY" rather than folded into it: a set that was switched off is not a
			// set that could not elect anybody, and the numbers are read against each other.
			return render.Duration(summary.Stopped)
		})},
		{Title: "Unwatched", Align: render.AlignRight, Values: text(func(summary analyze.SetSummary) string {
			return render.Duration(summary.Unwatched)
		})},
		{Title: "History", Values: text(func(summary analyze.SetSummary) string {
			if summary.Partial {
				return "partial"
			}
			return "complete"
		})},
	}, options)
}

// memberLabel is what to call a member on its band row: the log it is, where the bundle has one,
// and the name its set knows it by where it does not.
func memberLabel(member analyze.MemberHistory) string {
	// The name the set knows it by comes first, whether or not the bundle holds its log: a node's
	// log is rotated, so "the log that is this member" is one of several files and naming a member
	// after whichever one sorted first says nothing a reader wants.
	if member.HostAndPort != "" {
		return shortHost(member.HostAndPort)
	}
	if member.HasLog() {
		return member.Source
	}
	// The domain comes off before the column trims anything. Every member of a set shares all of
	// its name but the first label, and a column that keeps the end of the text left two rows both
	// reading "…db.example.com:27017" — two members of one set that nothing could tell apart.
	return shortHost(member.HostAndPort)
}

// setTitle names a replica set, saying plainly when a member never said which set it was in rather
// than printing an empty heading.
func setTitle(name, raw string) string {
	if raw == "" {
		return "replica set not logged"
	}
	return name
}

// setsOf is the replica sets the members belong to, in the order they should be drawn.
func setsOf(members []analyze.MemberHistory) []string {
	seen := map[string]bool{}
	sets := make([]string, 0, 4)
	for _, member := range members {
		if !seen[member.Set] {
			seen[member.Set] = true
			sets = append(sets, member.Set)
		}
	}
	return sets
}

// bandSpans is the drawing's view of a history: an unknown span carries no state, which is what
// makes the chart draw it as unknown rather than as whatever was last true.
func bandSpans(spans []analyze.StateSpan) []render.BandSpan {
	out := make([]render.BandSpan, 0, len(spans))
	for _, span := range spans {
		state := span.State
		if !span.Known() {
			state = ""
		}
		out = append(out, render.BandSpan{From: span.From, To: span.To, State: state})
	}
	return out
}

// shortHost drops the domain from a replica set member's name, keeping the host and the port.
//
// A config spells its members in full — "rs0-1.db.example.com:27019" — and every
// member of a set shares all of that but the first label. Most of the characters are the same on
// every row, in a column beside a set name that was being squeezed to "rs…" to make room for them.
// The row already says which set it is, so the label and the port are the whole of what identifies
// a member inside it.
func shortHost(name string) string {
	host, port, found := strings.Cut(name, ":")
	if !found {
		return name
	}
	if label, _, hasDomain := strings.Cut(host, "."); hasDomain {
		host = label
	}
	return host + ":" + port
}
