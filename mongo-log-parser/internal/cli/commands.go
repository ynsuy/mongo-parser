package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"mongo-log-parser/internal/analyze"
	"mongo-log-parser/internal/render"
)

// The three commands here each show one section of the report in full. The report shows the head of
// each; this is where a reader goes once they know which one they are looking at.
//
// They each make one pass over the stored lines, which is all any of them needs: the store was
// filled once, and analyze.Run offers every line the window admits to every accumulator at once.

// runDeployment answers "what am I looking at": the versions, the roles, the cache sizes and the
// restarts. It is the first question of every triage and the one a bundle states nowhere obvious —
// the facts are in a handful of startup lines buried in gigabytes of ordinary traffic.
func runDeployment(root string, args []string) error {
	flags, c := newFlags("deployment")
	w := addWindowFlags(flags)
	color := flags.String("color", "never", "never (default), auto or always")
	if err := parseOnly(flags, args); err != nil {
		return err
	}
	started := time.Now()
	st, err := openStore(root, c.refresh)
	if err != nil {
		return err
	}
	defer st.Close()
	start, end, err := w.resolve(st, time.Time{})
	if err != nil {
		return err
	}
	q, err := readQuality(st)
	if err != nil {
		return err
	}

	// The replication lines are read alongside the startup lines, because a log rotated past the
	// last restart carries the set name and nothing else. Which rotations are one node was settled
	// when the bundle was read, so there is nothing to group here.
	deployment, cluster := analyze.NewDeployment(), analyze.NewCluster()
	if err := analyze.Run(st, windowQuery(start, end), deployment, cluster); err != nil {
		return err
	}
	deployment.Learn(cluster)
	joinRuns(st, deployment, start, end)

	processes := deployment.Processes()
	summary := deployment.Summarize()
	options := render.TableOptions{Width: outputWidth(c.width), Color: useColor(*color) && !c.json}
	rendered := renderProcesses(processes, options)
	if line := deploymentLine(summary); line != "" {
		rendered = line + "\n\n" + rendered
	}
	if history := versionHistory(processes); history != "" {
		rendered += "\n\n" + history
	}
	if restarts := restartLines(processes); restarts != "" {
		rendered += "\n\n" + restarts
	}
	for _, part := range []string{changeLines(processes), endingLines(processes), systemLines(deployment.SystemEvents())} {
		if part != "" {
			rendered += "\n\n" + part
		}
	}
	footer := fmt.Sprintf("%sthe bundle holds %s in %s; answered in %s",
		askedWindow(start, end),
		plural(q.Parsed, "record"), plural(q.Files, "log stream"), time.Since(started).Round(time.Millisecond))
	return emit(os.Stdout, c.json, "deployment", root, map[string]any{
		"processes": processes, "summary": summary, "system_events": deployment.SystemEvents(), "quality": q,
	}, withVersion(rendered+"\n\n"+footer))
}

// restartLines says when the processes came up, because a counter that resets is the first thing to
// know before reading any counter at all.
func restartLines(processes []analyze.Process) string {
	lines := make([]string, 0, 4)
	for _, process := range processes {
		if len(process.StartedAt) < 2 {
			continue
		}
		stamps := make([]string, 0, len(process.StartedAt))
		for _, start := range process.StartedAt {
			stamps = append(stamps, start.At.Format("01-02 15:04"))
		}
		lines = append(lines, fmt.Sprintf("%s started at: %s",
			process.Source, strings.Join(stamps, ", ")))
	}
	if len(lines) == 0 {
		return ""
	}
	return "restarts inside the window\n" + strings.Join(lines, "\n")
}

// runDDL shows the catalogue changes: the index builds with their durations, and everything else
// with its timing. A build that overlapped a bad window is the cheapest explanation there is, and
// it is invisible in every metric.
func runDDL(root string, args []string) error {
	flags, c := newFlags("ddl")
	w := addWindowFlags(flags)
	namespace := flags.String("ns", "", "only this namespace")
	kind := flags.String("kind", "", "only this kind of change, e.g. \"drop collection\"")
	top := flags.Int("top", 25, "how many rows of each table to show")
	color := flags.String("color", "never", "never (default), auto or always")
	if err := parseOnly(flags, args); err != nil {
		return err
	}
	if err := bounded(bound{"top", *top, 1}); err != nil {
		return err
	}
	started := time.Now()
	st, err := openStore(root, c.refresh)
	if err != nil {
		return err
	}
	defer st.Close()
	start, end, err := w.resolve(st, time.Time{})
	if err != nil {
		return err
	}
	q, err := readQuality(st)
	if err != nil {
		return err
	}

	catalogue := analyze.NewDDL()
	if err := analyze.Run(st, windowQuery(start, end), catalogue); err != nil {
		return err
	}

	builds := make([]analyze.IndexBuild, 0, 16)
	for _, build := range catalogue.Builds() {
		if *namespace != "" && build.Namespace != *namespace {
			continue
		}
		if *kind != "" && !strings.EqualFold(*kind, "index build") {
			continue
		}
		builds = append(builds, build)
	}
	changes := make([]analyze.Change, 0, 16)
	for _, change := range catalogue.Changes() {
		if *namespace != "" && change.Namespace != *namespace {
			continue
		}
		if *kind != "" && !strings.EqualFold(*kind, change.Kind) {
			continue
		}
		changes = append(changes, change)
	}
	if len(builds) == 0 && len(changes) == 0 {
		// Still through emit: a caller asking for JSON gets a document saying nothing matched,
		// rather than a sentence on stdout that its parser chokes on.
		return emit(os.Stdout, c.json, "ddl", root, map[string]any{
			"index_builds": builds, "changes": changes, "counts": catalogue.Counts(), "quality": q,
		}, withVersion("no catalogue changes matched\n\n"+countsNote(catalogue.Counts())))
	}

	options := render.TableOptions{Width: outputWidth(c.width), Color: useColor(*color) && !c.json}
	parts := make([]string, 0, 2)
	if len(builds) > 0 {
		parts = append(parts, "index builds\n"+renderBuilds(builds, *top, options))
	}
	if len(changes) > 0 {
		parts = append(parts, "other catalogue changes\n"+renderChanges(changes, *top, options))
	}
	footer := fmt.Sprintf("%s%s; the bundle holds %s in %s; answered in %s",
		askedWindow(start, end),
		countsNote(catalogue.Counts()), plural(q.Parsed, "record"), plural(q.Files, "log stream"),
		time.Since(started).Round(time.Millisecond))
	return emit(os.Stdout, c.json, "ddl", root, map[string]any{
		"index_builds": builds, "changes": changes,
		"counts": catalogue.Counts(), "failed_builds": catalogue.FailedBuilds(), "quality": q,
	}, withVersion(strings.Join(parts, "\n\n")+"\n\n"+footer))
}

// countsNote is the whole catalogue in one clause, so a filtered view still says what it filtered.
func countsNote(counts map[string]int) string {
	if len(counts) == 0 {
		return "no catalogue changes in this window"
	}
	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	// Busiest first; the tie-break on name keeps two runs of the same bundle identical.
	sort.Slice(kinds, func(i, j int) bool {
		if counts[kinds[i]] != counts[kinds[j]] {
			return counts[kinds[i]] > counts[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, fmt.Sprintf("%s×%d", kind, counts[kind]))
	}
	return "in the whole window: " + strings.Join(parts, " ")
}

// runHealth shows the conditions the server logged without ever raising a severity for them: the
// authentication failures, the write conflicts, the connection churn, the heartbeats that missed.
// None of them is an error line, so nothing that ranks by severity ever shows them.
func runHealth(root string, args []string) error {
	flags, c := newFlags("health")
	w := addWindowFlags(flags)
	top := flags.Int("top", 30, "how many conditions to show")
	color := flags.String("color", "never", "never (default), auto or always")
	if err := parseOnly(flags, args); err != nil {
		return err
	}
	if err := bounded(bound{"top", *top, 1}); err != nil {
		return err
	}
	started := time.Now()
	st, err := openStore(root, c.refresh)
	if err != nil {
		return err
	}
	defer st.Close()
	start, end, err := w.resolve(st, time.Time{})
	if err != nil {
		return err
	}
	q, err := readQuality(st)
	if err != nil {
		return err
	}

	health := analyze.NewHealth()
	if err := analyze.Run(st, windowQuery(start, end), health); err != nil {
		return err
	}
	signals := health.Signals()
	if len(signals) == 0 {
		return emit(os.Stdout, c.json, "health", root, map[string]any{"signals": signals, "quality": q},
			withVersion("no named conditions fired in this window"))
	}
	options := render.TableOptions{Width: outputWidth(c.width), Color: useColor(*color) && !c.json}
	rendered := renderSignals(signals, *top, options)
	if opened, closed, perMinute := health.ConnectionChurn(); opened > 0 {
		rendered += fmt.Sprintf("\n\nconnections: %d opened, %d closed, %s per minute while they were being opened",
			opened, closed, render.Compact(perMinute))
	}
	footer := fmt.Sprintf("%sthe bundle holds %s in %s; answered in %s\nEach condition is a pattern in the message text, not a verdict; read the lines before acting on one.",
		askedWindow(start, end),
		plural(q.Parsed, "record"), plural(q.Files, "log stream"), time.Since(started).Round(time.Millisecond))
	return emit(os.Stdout, c.json, "health", root, map[string]any{
		"signals": signals, "quality": q,
	}, withVersion(rendered+"\n\n"+footer))
}
