package cli

import (
	"fmt"
	"mongo-log-parser/internal/analyze"
	"sort"
	"strings"
	"time"

	"mongo-log-parser/internal/funnel"
	"mongo-log-parser/internal/label"
	"mongo-log-parser/internal/render"
)

// renderStall is the first look at a bundle's labels: the ones worth opening, the ones worth
// noticing, and a count of the ordinary rest. Each one worth opening names the command that opens
// it; the ordinary ones are in --json, with what every band means.
func renderStall(stall funnel.Stall, root string, options render.TableOptions, cluster *analyze.Cluster) string {
	parts := make([]string, 0, 6)
	if len(stall.Open) > 0 {
		parts = append(parts, fmt.Sprintf("Worth opening (%d)\n%s", len(stall.Open), stallTable(stall.Open, options)))
	}
	if len(stall.Notice) > 0 {
		parts = append(parts, fmt.Sprintf("Worth noticing (%d)\n%s", len(stall.Notice), stallTable(stall.Notice, options)))
	}
	if text := memberNote(append(stall.Open, stall.Notice...), cluster); text != "" {
		parts = append(parts, text)
	}
	if len(stall.Open)+len(stall.Notice) > 0 {
		parts = append(parts, fmt.Sprintf("Open one, second by second, beside the labels it pairs with and the logs of the same minutes:\n  mongo-log-parser label %s <label> --source <node>", root))
	}
	if line := ordinaryLine(stall.Ordinary); line != "" {
		parts = append(parts, line)
	}
	if text := renderImbalances(stall.Imbalances, options); text != "" {
		parts = append(parts, text)
	}
	if text := skippedNote(stall.Skipped); text != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n")
}

func stallTable(items []funnel.Item, options render.TableOptions) string {
	text := func(read func(funnel.Item) string) []string {
		values := make([]string, len(items))
		for i, item := range items {
			values[i] = read(item)
		}
		return values
	}
	return render.Table([]render.Column{
		{Title: "Node", Values: text(func(i funnel.Item) string { return i.Source }), Elastic: true},
		{Title: "Label", Values: text(func(i funnel.Item) string { return strings.TrimSpace(i.Label + " " + i.Instance) })},
		{Title: "Usual", Values: text(func(i funnel.Item) string { return levelReading(i.Result, i.Typical) })},
		{Title: "Worst minute", Values: text(func(i funnel.Item) string { return levelReading(i.Result, i.Peak) })},
		{Title: "Worst second", Values: text(worstSecond)},
		{Title: "When", Values: text(func(i funnel.Item) string { return when(i.Result) })},
		{Title: "Why", Values: text(func(i funnel.Item) string { return strings.Join(i.Why, "; ") })},
		{Title: "Moved with", Values: text(func(i funnel.Item) string { return movedWith(i.Result) })},
	}, options)
}

// memberNote says which replica set member each numbered instance above is, and which members the
// config has besides: an arbiter or a member no label is about still decides what a majority is.
func memberNote(items []funnel.Item, cluster *analyze.Cluster) string {
	lines, seen := make([]string, 0, 8), map[string]bool{}
	named := map[string]map[string]bool{}
	for _, i := range items {
		key := i.Source + " " + i.Instance
		if i.Member == "" || seen[key] {
			continue
		}
		seen[key] = true
		if named[i.Source] == nil {
			named[i.Source] = map[string]bool{}
		}
		named[i.Source][i.Member] = true
		lines = append(lines, fmt.Sprintf("  %s instance %s = %s", i.Source, i.Instance, i.Member))
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	var rest []string
	if cluster != nil {
		for source, members := range named {
			for _, m := range cluster.ConfigMembers(source) {
				if !members[m.String()] {
					rest = append(rest, fmt.Sprintf("  %s config also has %s", source, m))
				}
			}
		}
	}
	sort.Strings(rest)
	return "Members, from the node's logged replica set config:\n" + strings.Join(append(lines, rest...), "\n")
}

// ordinaryLine counts the labels that did nothing remarkable, node by node.
func ordinaryLine(counts []funnel.NodeCount) string {
	if len(counts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(counts))
	for _, n := range counts {
		parts = append(parts, fmt.Sprintf("%s %d", n.Source, n.Count))
	}
	return "Ordinary — stayed in their lowest band and did nothing remarkable: " + strings.Join(parts, ", ") +
		". Every one, with what its bands mean, is in --json."
}

// levelReading is a value in the label's unit with the band it belongs to most.
func levelReading(r label.Result, value float64) string {
	band, _ := r.Rule().Strongest(value)
	return strings.TrimSpace(amount(value, r.Unit) + " " + band)
}

// worstSecond is the signal at its most extreme single second, and for a time view how many
// seconds of the window were past the threshold in all.
func worstSecond(i funnel.Item) string {
	if i.PeakSecondAt.IsZero() {
		return "-"
	}
	text := secondAt(i.PeakSecond, i.SignalUnit, i.PeakSecondAt)
	if i.Rule().Timed() && i.PastSeconds > 0 {
		text += fmt.Sprintf(", %s past in all", duration(int64(i.PastSeconds)*1000))
	}
	return text
}

// secondAt is a signal's value at one second. An indicator's value is a code, not an amount, so
// only the moment is printed.
func secondAt(value float64, unit string, at time.Time) string {
	stamp := at.Format("01-02 15:04:05")
	if unit == "state" || unit == "running" {
		return "at " + stamp
	}
	return amount(value, unit) + " at " + stamp
}

func amount(value float64, unit string) string {
	switch unit {
	case "%", "% available":
		if value < 10 {
			return fmt.Sprintf("%.1f%s", value, unit)
		}
		return fmt.Sprintf("%.0f%s", value, unit)
	case label.TimeUnit:
		return fmt.Sprintf("%.0f %s", value, unit)
	case "":
		return render.Compact(value)
	}
	return render.Compact(value) + " " + unit
}

// when is the moment a reader lines up against the log: the start of the largest episode, the
// moment of a step, or the worst minute.
func when(r label.Result) string {
	const stamp = "01-02 15:04"
	switch {
	case r.Periodic != nil:
		p := r.Periodic
		return fmt.Sprintf("every %s, %d times, %s to %s", p.Every, p.Occurrences, p.First.Format(stamp), p.Last.Format(stamp))
	case r.Change != nil:
		return fmt.Sprintf("%s %s → %s", r.Change.At.Format(stamp), amount(r.Change.Before, r.Unit), amount(r.Change.After, r.Unit))
	case len(r.Episodes) > 0:
		e := r.Episodes[0]
		held := fmt.Sprintf("%dm", e.Minutes)
		if e.Open {
			held += "+"
		}
		more := ""
		if len(r.Episodes) > 1 {
			more = fmt.Sprintf(" (+%d more)", len(r.Episodes)-1)
		}
		return fmt.Sprintf("%s for %s at %s, usually %s%s", e.Start.Format(stamp), held, amount(e.Mean, r.Unit), amount(e.Baseline, r.Unit), more)
	}
	return "worst " + r.PeakAt.Format(stamp)
}

// movedWith names the two labels that moved most closely with this one, and which went first.
func movedWith(r label.Result) string {
	names := make([]string, 0, 2)
	for _, rel := range r.Related[:min(2, len(r.Related))] {
		name := strings.TrimSpace(rel.Label + " " + rel.Instance)
		switch {
		case rel.R < 0:
			name += " (opposite)"
		case rel.Lead > 0:
			name += fmt.Sprintf(" (%dm before)", rel.Lead)
		case rel.Lead < 0:
			name += fmt.Sprintf(" (%dm after)", -rel.Lead)
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// renderImbalances lists what was not alike: one disk of several, one node of a set.
func renderImbalances(found []label.Imbalance, options render.TableOptions) string {
	if len(found) == 0 {
		return ""
	}
	text := func(read func(label.Imbalance) string) []string {
		values := make([]string, len(found))
		for i, im := range found {
			values[i] = read(im)
		}
		return values
	}
	return "Not alike:\n" + render.Table([]render.Column{
		{Title: "Label", Values: text(func(im label.Imbalance) string { return im.Label })},
		{Title: "Across", Values: text(func(im label.Imbalance) string {
			if im.Source != "" {
				return fmt.Sprintf("%d devices on %s", im.Count, im.Source)
			}
			if im.Peers != "" {
				return fmt.Sprintf("%d nodes, each %s", im.Count, im.Peers)
			}
			return fmt.Sprintf("%d nodes", im.Count)
		})},
		{Title: "Hottest", Values: text(func(im label.Imbalance) string { return im.Hottest })},
		{Title: "p95", Align: render.AlignRight, Values: text(func(im label.Imbalance) string { return render.Compact(im.Value) })},
		{Title: "Others", Align: render.AlignRight, Values: text(func(im label.Imbalance) string { return render.Compact(im.Others) })},
		{Title: "Ratio", Values: text(func(im label.Imbalance) string { return fmt.Sprintf("%.1fx (%s)", im.Ratio, im.Comparison) })},
	}, options)
}

// skippedNote says which labels a node could not have and why, grouped by reason, so a missing label
// is never read as a quiet metric.
func skippedNote(skipped []label.Skip) string {
	if len(skipped) == 0 {
		return ""
	}
	byReason := map[string][]string{}
	order := make([]string, 0, 8)
	for _, s := range skipped {
		key := s.Source + ": " + s.Reason
		if byReason[key] == nil {
			order = append(order, key)
		}
		byReason[key] = append(byReason[key], s.Label)
	}
	lines := make([]string, 0, len(order)+1)
	lines = append(lines, fmt.Sprintf("Not labelled (%d):", len(skipped)))
	for _, key := range order {
		lines = append(lines, "  "+key+" — "+strings.Join(byReason[key], ", "))
	}
	return strings.Join(lines, "\n")
}

// openCommand is the command that opens one label on one node.
func openCommand(root string) func(label.Result) string {
	return func(r label.Result) string {
		command := fmt.Sprintf("mongo-log-parser label %s %s --source %s", root, r.Label, r.Source)
		if r.Instance != "" {
			command += " --instance " + r.Instance
		}
		return command
	}
}
