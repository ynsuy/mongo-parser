package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ynsuy/mongo-parser/internal/metrics"
	"github.com/ynsuy/mongo-parser/internal/render"
	"github.com/ynsuy/mongo-parser/internal/series"
)

func runMetrics(root string, args []string) error {
	flags, c := newFlags("metrics")
	var wanted stringList
	flags.Var(&wanted, "source", "stream to read; repeat for several, or \"all\"; every stream by default")
	top := flags.Int("top", 12, "how many metrics to draw when none are named")
	var named stringList
	flags.Var(&named, "metric", "metric path or name, * and ? matching several; repeat for more")
	w := addWindowFlags(flags)
	binSeconds := flags.Int64("bin", 0, "bin size in seconds; chosen from the window by default")
	height := flags.Int("height", chartHeight, "bar rows per stream")
	list := flags.Bool("list", false, "list the metrics worth charting, by group, instead of drawing them")
	words, err := parse(flags, args)
	if err != nil {
		return err
	}
	if err := bounded(bound{"top", *top, 1}, bound{"height", *height, 1}, bound{"bin", int(*binSeconds), 0}); err != nil {
		return err
	}

	started := time.Now()
	st, err := openStore(root, c.refresh)
	if err != nil {
		return err
	}
	defer st.Close()
	set, err := openStreams(st, wanted)
	if err != nil {
		return err
	}
	// A bundle with no diagnostic data has no metrics at all, and saying so plainly is the answer:
	// an empty board reads as a quiet deployment, which is the opposite of what happened.
	if len(set.streams) == 0 {
		return emit(os.Stdout, c.json, "metrics", root, []series.Series{},
			withVersion("no FTDC in this bundle, so there are no metrics to chart: diagnostic.data is what carries them, and nothing here holds one.\nWhat the logs say is in: report, slow, logs, health."), []string{"No FTDC in this bundle; metrics are unavailable, not zero."})
	}
	start, end, err := w.resolve(st, set.last)
	if err != nil {
		return err
	}
	// Words after the path name what to draw: a board, a panel, a metric, or part of a path. They
	// used to be parsed off and dropped, so `metrics <path> tcmalloc` quietly drew the standard
	// board instead — the worst kind of wrong answer, because it looks like the right one. They are
	// matched against every metric the streams carry, not only the ones decoded by default:
	// matching against the decoded set made the network counters, and every other column outside
	// the curated set, impossible to ask for.
	available := set.available(start, end)
	picked, err := selectMetrics(set, words, available, *top)
	if err != nil {
		return err
	}
	samples, order, err := set.read(start, end, named, picked, nil)
	if err != nil {
		return err
	}
	if len(picked) > 0 {
		order = picked
	}
	if *list {
		return emit(os.Stdout, c.json, "metric_list", root, catalogue(available), withVersion(listing(available)))
	}
	// With nothing named, the standard set is what gets drawn: the same board for every node and
	// every window, so two of them can be read against each other.
	if len(named) == 0 && len(picked) == 0 && len(order) > *top {
		order = order[:*top]
	}
	// A name that matched nothing is a different answer from a flat line, and has to say so. The
	// check runs on what the names resolved to, not what was typed, or a short name that resolved
	// cleanly would be reported as missing.
	if err := set.reportUnknownMetrics(resolvedNames(named, order), samples, start, end); err != nil {
		return err
	}

	// The plot sits inside the table, past the value-axis gutter; bins are chosen to fill it.
	plotWidth := render.BarColumns(outputWidth(c.width))
	options := metrics.Options{From: start, To: end, BucketSeconds: *binSeconds, TargetBins: plotWidth}
	perMetric := make(map[string][]series.Series, len(order))
	for _, path := range order {
		for i := range set.streams {
			built := metrics.Build(samples[i], set.sources[i], path, options)
			built.Label = set.label(path)
			perMetric[path] = append(perMetric[path], built)
		}
	}
	// A metric's streams stay together, so one node's line is read beside the others'.
	drawn := make([]series.Series, 0, len(order)*len(set.streams))
	for _, path := range order {
		drawn = append(drawn, perMetric[path]...)
	}
	if len(drawn) == 0 {
		return fmt.Errorf("no metrics matched; try --metric or a wider window")
	}

	// A panel per metric, a strip per stream on one scale.
	rendered := render.BarChart(drawn, render.BarOptions{Width: outputWidth(c.width), Height: *height})
	var notes []string
	footer := fmt.Sprintf("%d chunks indexed across %s; %s; decode and render in %s",
		set.chunks, plural(len(set.streams), "stream"), strings.Join(set.spans(), "; "), time.Since(started).Round(time.Millisecond))
	if damage := damageNote(samples, set.sources); damage != "" {
		notes = append(notes, damage)
		footer = damage + "\n\n" + footer
	}
	return emit(os.Stdout, c.json, "metrics", root, drawn, withVersion(rendered+"\n\n"+footer), notes)
}

// group is a set of metrics filed under one subject, for the listing.
type group struct {
	Name  string   `json:"name"`
	Paths []string `json:"paths"`
}

// catalogue is what this bundle carries, filed by subject, so a caller can see what there is to ask
// for before asking. Every metric is in it: the curated ones under the subjects a triage walks, the
// rest under the subtree they come from, after them.
func catalogue(available []string) []group {
	byFamily := map[string][]string{}
	extra := make([]string, 0, 16)
	for _, path := range available {
		family := metrics.Family(path)
		if family == "" {
			family = subtree(path)
			if byFamily[family] == nil {
				extra = append(extra, family)
			}
		}
		byFamily[family] = append(byFamily[family], path)
	}
	sort.Strings(extra)
	groups := make([]group, 0, len(byFamily))
	for _, family := range append(metrics.Families(), extra...) {
		if paths := byFamily[family]; len(paths) > 0 {
			sort.Strings(paths)
			groups = append(groups, group{Name: family, Paths: paths})
			delete(byFamily, family)
		}
	}
	return groups
}

// subtree is the part of a path that says where an uncurated metric comes from:
// serverStatus/wiredTiger, systemMetrics/netstat, local.oplog.rs.stats.
func subtree(path string) string {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 3 {
		return parts[0]
	}
	return parts[0] + "/" + parts[1]
}

// listing names the metrics this bundle carries: the panels a board is made of, because those are
// the questions someone usually means, then everything else by subject.
func listing(available []string) string {
	out := make([]string, 0, 128)
	out = append(out, "PANELS — the standard boards, and what --metric is usually meant to reach")
	for _, board := range metrics.Boards() {
		shown := make([]string, 0, len(board.Panels))
		for _, panel := range board.Panels {
			lines := panel.Resolve(available)
			if len(lines) == 0 {
				continue
			}
			names := make([]string, 0, len(lines))
			for _, line := range lines {
				name := line.Label
				if name == "" {
					name = render.MetricName(line.Path)
				}
				if line.Per != "" {
					name += " (per op)"
				}
				if len(line.Of) > 0 {
					name += " (%)"
				}
				names = append(names, name)
			}
			shown = append(shown, fmt.Sprintf("    %-38s %s", panel.Name, strings.Join(names, ", ")))
		}
		if len(shown) > 0 {
			out = append(out, "  "+board.Name)
			out = append(out, shown...)
		}
	}

	out = append(out, "", "EVERY METRIC")
	total := 0
	for _, g := range catalogue(available) {
		out = append(out, fmt.Sprintf("  %s (%d)", g.Name, len(g.Paths)))
		for _, path := range g.Paths {
			out = append(out, "      "+path)
			total++
		}
	}
	out = append(out, "", fmt.Sprintf("%d metrics. Chart one by naming it after the path, or with --metric.", total))
	return strings.Join(out, "\n")
}

// selectMetrics turns the words after the path into the metrics to draw.
//
// A person asking about the heap types "tcmalloc", not a full FTDC path, and they are right to: the
// tool already knows the boards, the panels and every short name it printed, so making them type
// what it can look up is making them do its work. Each word is tried against the things it might
// mean, from the most specific to the least, and a word that means nothing is an error with the
// nearest names rather than a silent fall back to something else.
func selectMetrics(set *streamSet, words, available []string, limit int) ([]string, error) {
	if len(words) == 0 {
		return nil, nil
	}
	held := make(map[string]bool, len(available))
	for _, path := range available {
		held[path] = true
	}
	order := make([]string, 0, len(words)*4)
	seen := map[string]bool{}
	// keep reports how many of these paths the bundle carries, not how many were new. A word whose
	// every match a previous word already brought has still been understood, and reporting it as
	// nothing would say the word was wrong when the answer is already on the board.
	keep := func(paths ...string) int {
		hits := 0
		for _, path := range paths {
			if !held[path] {
				continue
			}
			hits++
			if !seen[path] {
				seen[path] = true
				order = append(order, path)
			}
		}
		return hits
	}

	for _, word := range words {
		lower, hits := strings.ToLower(word), 0

		// A pattern stands for every metric it matches: document/* is a reader asking for the
		// subtree they were just shown, not for a metric with a star in its name.
		if hasGlob(word) {
			matched, err := globPaths(word, available, set.label)
			if err != nil {
				return nil, err
			}
			keep(matched...)
			continue
		}

		// A path, or a name this tool printed for one.
		hits += keep(word, set.metricPath(word))

		// A board, or a panel on any board. A board is the whole screen a standard view would show;
		// a panel is one chart of it, and either is a reasonable thing to have in mind.
		for _, board := range metrics.Boards() {
			named := strings.EqualFold(board.Name, word)
			for _, panel := range board.Panels {
				if !named && !strings.Contains(strings.ToLower(panel.Name), lower) {
					continue
				}
				for _, line := range panel.Resolve(available) {
					hits += keep(line.Path)
				}
			}
		}

		// Failing that, any metric whose path says the word. Capped, because a word like "bytes"
		// answers to hundreds and a board of hundreds is not an answer.
		if hits == 0 {
			for _, path := range available {
				if hits >= limit {
					break
				}
				if strings.Contains(strings.ToLower(path), lower) {
					hits += keep(path)
				}
			}
		}

		if hits == 0 {
			message := fmt.Sprintf("nothing called %q: no board, no panel, no metric", word)
			if nearest := suggest(available, word, 6); len(nearest) > 0 {
				message += "; did you mean:\n  " + strings.Join(nearest, "\n  ")
			} else {
				message += "; run with --list to see the boards, the panels and every metric"
			}
			return nil, fmt.Errorf("%s", message)
		}
	}
	return order, nil
}
