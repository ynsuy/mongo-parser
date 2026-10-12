package cli

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"mongo-log-parser/internal/funnel"
	"mongo-log-parser/internal/label"
	"mongo-log-parser/internal/render"
	"mongo-log-parser/internal/timeparse"
)

// runLabel opens one label up: the step after the stall. It says exactly when the label was past
// its mark, second by second, reads every label it pairs with over those same seconds, names the
// labels that moved with it, and says what the node's log said in the same window. With --at it
// prints a window second by second, the label and its pairs side by side.
func runLabel(root string, args []string) error {
	fs, c := newFlags("label")
	w := addWindowFlags(fs)
	source := fs.String("source", "", "the node to open it on; every node that has it when empty")
	instance := fs.String("instance", "", "the disk or member, for a label that has them")
	atText := fs.String("at", "", "a moment to read second by second (UTC)")
	span := fs.Duration("span", 4*time.Minute, "how much around --at to read second by second")
	color := fs.String("color", "never", "never (default), auto or always")
	words, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(words) != 1 {
		return fmt.Errorf("name one label to open, e.g. tickets.read.empty; every label is listed by `mongo-log-parser labels`")
	}
	name := words[0]
	if len(label.Definitions(name)) == 0 {
		return fmt.Errorf("no label is called %s; %s", name, nearestLabels(name))
	}
	if *span <= 0 || *span > 15*time.Minute {
		return fmt.Errorf("--span is how much to print second by second: more than nothing and at most 15m")
	}

	st, err := openStore(root, c.refresh)
	if err != nil {
		return err
	}
	defer st.Close()
	wanted := []string{}
	if *source != "" {
		wanted = append(wanted, *source)
	}
	set, err := openStreams(st, wanted)
	if err != nil {
		return err
	}
	if len(set.streams) == 0 {
		return emit(os.Stdout, c.json, "label", root, []openedEntry{}, withVersion("no FTDC in this bundle, so there are no labels to open"), []string{"No FTDC in this bundle; labels are unavailable, not ordinary."})
	}
	start, end, err := w.resolve(st, set.last)
	if err != nil {
		return err
	}
	var at time.Time
	if *atText != "" {
		first, last := logSpan(st)
		if set.first.Before(first) || first.IsZero() {
			first = set.first
		}
		if set.last.After(last) {
			last = set.last
		}
		if at, err = timeparse.Parse(*atText, first, last); err != nil {
			return fmt.Errorf("--at: %w", err)
		}
	}

	// Every node FTDC names, not only the ones asked about: a log matched by a short name is only
	// as certain as the number of nodes that share it.
	streams, err := ftdcStreams(st, nil)
	if err != nil {
		return err
	}
	everyNode := make([]string, len(streams))
	for i, f := range streams {
		everyNode[i] = f.label
	}
	options := render.TableOptions{Width: outputWidth(c.width), Color: useColor(*color) && !c.json}
	opened := make([]funnel.Opened, 0, len(set.streams))
	sections, notes := make([]string, 0, 4), make([]string, 0, 4)
	for i, stream := range set.streams {
		node := set.sources[i]
		samples, err := stream.Read(start, end, label.Wants, nil)
		if err != nil {
			notes = append(notes, node+": "+err.Error())
			continue
		}
		if damage := damageLine(samples, node); damage != "" {
			notes = append(notes, damage)
		}
		results, skipped := label.Run(samples, node, stream.Identity().Version, start, end)
		label.Relate(results)
		found := false
		for _, r := range results {
			if r.Label != name || (*instance != "" && r.Instance != *instance) {
				continue
			}
			found = true
			o := funnel.Open(r, results, samples, start, end, at, *span)
			for j := range o.Focus {
				e, err := readEvidence(st, root, node, everyNode, o.Focus[j].From, o.Focus[j].To)
				if err != nil {
					return err
				}
				o.Focus[j].Evidence = e
			}
			opened = append(opened, o)
			sections = append(sections, renderOpened(o, options))
		}
		if !found {
			notes = append(notes, node+": "+whyNotLabelled(name, *instance, skipped))
		}
	}
	text := strings.Join(sections, "\n\n")
	if len(notes) > 0 {
		text = strings.TrimSpace(text + "\n\n" + strings.Join(notes, "\n"))
	}
	entries := make([]openedEntry, 0, len(opened))
	for _, o := range opened {
		entries = append(entries, openedEntry{Opened: o, Rule: catalogueEntry(o.Result.Rule())})
	}
	return emit(os.Stdout, c.json, "label", root, entries, withVersion(text), notes)
}

// openedEntry is one opened label with the definition it was read against, so the reading travels
// with what the rule measures, its reference and when it applies.
type openedEntry struct {
	funnel.Opened
	Rule ruleEntry `json:"rule"`
}

// whyNotLabelled says why a node has no reading of the label asked for.
func whyNotLabelled(name, instance string, skipped []label.Skip) string {
	for _, s := range skipped {
		if s.Label == name {
			return name + " is not labelled here: " + s.Reason
		}
	}
	if instance != "" {
		return fmt.Sprintf("%s has no instance %s here", name, instance)
	}
	return name + " is not labelled here"
}

// nearestLabels names the labels that share a word with what was typed.
func nearestLabels(name string) string {
	near := make([]string, 0, 8)
	for _, known := range label.Names() {
		for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '.' || r == '_' }) {
			if len(part) > 2 && strings.Contains(known, part) {
				near = append(near, known)
				break
			}
		}
	}
	if len(near) == 0 {
		return "every label is listed by `mongo-log-parser labels`"
	}
	return "did you mean " + strings.Join(near[:min(6, len(near))], ", ")
}

// renderOpened is one opened label: what it is, what it did over the window, its stretches, and each
// focus window with its pairs, the labels that moved with it, and the log.
func renderOpened(o funnel.Opened, options render.TableOptions) string {
	r := o.Result
	rule := r.Rule()
	head := fmt.Sprintf("%s on %s", strings.TrimSpace(r.Label+" "+r.Instance), r.Source)
	lines := []string{head, strings.Repeat("─", min(len(head), 72)),
		rule.Title + ". " + strings.Join(strings.Fields(rule.Doc), " "),
	}
	if ref := referenceText(rule); ref != "" {
		lines = append(lines, "Reference: "+ref+".")
	}
	if rule.Applies != "" {
		lines = append(lines, "Applies: "+strings.Join(strings.Fields(rule.Applies), " "))
	}
	lines = append(lines, "",
		fmt.Sprintf("usual %s · worst minute %s at %s · worst second %s · %s",
			levelReading(r, r.Typical), levelReading(r, r.Peak), r.PeakAt.Format("15:04"), worstSecondOf(r), r.Shape),
	)
	if w := when(r); r.Shape != "steady" && r.Shape != "constant" {
		lines = append(lines, "when: "+w)
	}
	if rel := movedWith(r); rel != "" {
		lines = append(lines, "moved with: "+rel)
	}
	if len(o.Stretches) > 0 {
		heading := fmt.Sprintf("Seconds past the mark (%s): %s", markText(rule), plural(o.StretchCount, "stretch"))
		if o.StretchCount > len(o.Stretches) {
			heading += fmt.Sprintf(", the longest %d", len(o.Stretches))
		}
		lines = append(lines, "", heading+":", stretchTable(o, options))
	} else if mark := markText(rule); mark != "" {
		lines = append(lines, "", "Never past the mark ("+mark+") in this window.")
	}
	for _, f := range o.Focus {
		lines = append(lines, "", renderFocus(o, f, options))
	}
	if len(o.Focus) > 0 && o.Focus[0].Seconds == nil {
		f := o.Focus[0]
		lines = append(lines, "", fmt.Sprintf("Second by second: add --at %s", f.From.Add(f.To.Sub(f.From)/2).Format("2006-01-02T15:04:05Z")))
	}
	return strings.Join(lines, "\n")
}

func worstSecondOf(r label.Result) string {
	if r.PeakSecondAt.IsZero() {
		return "-"
	}
	return fmt.Sprintf("%s at %s", amount(r.PeakSecond, r.SignalUnit), r.PeakSecondAt.Format("15:04:05"))
}

// markText is what counts a second towards a stretch, in words.
func markText(rule label.Rule) string {
	switch {
	case rule.Timed():
		return readOn(rule)
	case rule.Elevated == nil:
		return ""
	case rule.Direction == "down":
		return fmt.Sprintf("%s or less", amount(*rule.Elevated, rule.Signal.Unit))
	}
	return fmt.Sprintf("%s or more", amount(*rule.Elevated, rule.Signal.Unit))
}

func stretchTable(o funnel.Opened, options render.TableOptions) string {
	unit := o.Result.SignalUnit
	from, to, length, marked, worst := make([]string, 0), make([]string, 0), make([]string, 0), make([]string, 0), make([]string, 0)
	for _, s := range o.Stretches {
		from = append(from, s.From.Format("01-02 15:04:05"))
		to = append(to, s.To.Format("15:04:05"))
		length = append(length, s.Length().Round(time.Second).String())
		marked = append(marked, fmt.Sprintf("%d s", s.Seconds))
		worst = append(worst, fmt.Sprintf("%s at %s", amount(s.Worst, unit), s.WorstAt.Format("15:04:05")))
	}
	return render.Table([]render.Column{
		{Title: "From", Values: from}, {Title: "To", Values: to},
		{Title: "Length", Align: render.AlignRight, Values: length},
		{Title: "Past mark", Align: render.AlignRight, Values: marked},
		{Title: "Worst", Values: worst},
	}, options)
}

// renderFocus is one window: the label and its pairs over it against their usual, then the log.
func renderFocus(o funnel.Opened, f funnel.Focus, options render.TableOptions) string {
	lines := []string{fmt.Sprintf("%s → %s UTC — %s", f.From.Format("01-02 15:04:05"), f.To.Format("15:04:05"), f.Why)}
	names, here, usual := []string{}, []string{}, []string{}
	add := func(name string, rule label.Rule, unit string, window, normal label.Window, missing string) {
		names = append(names, name)
		switch {
		case missing != "":
			here, usual = append(here, missing), append(usual, "")
		default:
			here = append(here, windowText(rule, unit, window))
			usual = append(usual, windowText(rule, unit, normal))
		}
	}
	add(strings.TrimSpace(o.Result.Label+" "+o.Result.Instance), o.Result.Rule(), o.Result.SignalUnit, f.Self, label.Window{}, "")
	usual[0] = "-"
	for _, p := range f.Pairs {
		add(strings.TrimSpace(p.Label+" "+p.Instance), p.Rule(), p.SignalUnit, p.Window, p.Usual, p.Missing)
	}
	lines = append(lines, render.Table([]render.Column{
		{Title: "Label", Values: names},
		{Title: "In these seconds", Values: here},
		{Title: "Usually", Values: usual, Drop: 1},
	}, options))
	if len(f.Seconds) > 0 {
		lines = append(lines, "", secondsTable(f, options))
	}
	if e, ok := f.Evidence.(evidence); ok {
		lines = append(lines, "", evidenceText(e))
	}
	return strings.Join(lines, "\n")
}

// windowText is a label over a window in its signal's terms: its level and range, and for a time
// view how much of the window was past the threshold.
func windowText(rule label.Rule, unit string, w label.Window) string {
	if w.Samples == 0 {
		return "no seconds"
	}
	text := amount(w.Mean, unit)
	if math.Abs(w.Max-w.Min) >= 1e-9 {
		text = fmt.Sprintf("%s, %s to %s", text, render.Compact(w.Min), render.Compact(w.Max))
	}
	if rule.Timed() {
		text += fmt.Sprintf("; %d of %d s past", w.Past, w.Samples)
	}
	return text
}

// secondsTable prints a window second by second, a repeated row folded into the row before it.
func secondsTable(f funnel.Focus, options render.TableOptions) string {
	columns := f.Columns
	at := make([]string, len(f.Seconds))
	values := make([][]string, len(columns))
	for i, row := range f.Seconds {
		at[i] = row.At.Format("15:04:05")
		if row.Repeat > 0 {
			at[i] += fmt.Sprintf(" +%ds", row.Repeat)
		}
		for j, v := range row.Values {
			text := "-"
			if !math.IsNaN(v) {
				text = render.Compact(v)
			}
			values[j] = append(values[j], text)
		}
	}
	table := []render.Column{{Title: "Second", Values: at}}
	for j, name := range columns {
		table = append(table, render.Column{Title: name, Align: render.AlignRight, Values: values[j], Drop: len(columns) - j})
	}
	return render.Table(table, options)
}

// evidenceText is what the node's log said in the window, in a few lines.
func evidenceText(e evidence) string {
	if e.Lines == 0 {
		text := "The node's log has nothing in this window"
		if e.Elsewhere > 0 {
			text += fmt.Sprintf("; the other nodes logged %d lines", e.Elsewhere)
		}
		return text + "."
	}
	lines := []string{fmt.Sprintf("The node's log in this window: %d lines, %d errors, %d warnings; %d slow operations (%s ms).",
		e.Lines, e.Errors, e.Warnings, e.Slow, render.Compact(e.SlowMillis))}
	if e.Matched != "" {
		lines = append(lines, "  note: "+e.Matched+".")
	}
	for _, s := range e.SlowShapes {
		lines = append(lines, fmt.Sprintf("  slow: %s %s %s ×%d, %s ms (%s)", s.Namespace, s.Operation, s.Plan, s.Count, render.Compact(s.Millis), s.Hash))
	}
	if len(e.Waited) > 0 {
		parts := make([]string, 0, len(e.Waited))
		for _, w := range e.Waited {
			parts = append(parts, fmt.Sprintf("%s %.0f%%", w.Name, w.Share*100))
		}
		lines = append(lines, "  their time went on: "+strings.Join(parts, ", "))
	}
	for _, group := range []struct {
		name  string
		items []string
	}{{"conditions", e.Conditions}, {"replication", e.Replication}, {"index builds", e.IndexBuilds}} {
		if len(group.items) > 0 {
			items := append([]string(nil), group.items...)
			sort.Strings(items)
			lines = append(lines, "  "+group.name+": "+strings.Join(items[:min(5, len(items))], "; "))
		}
	}
	lines = append(lines, "  the lines: "+e.Next)
	return strings.Join(lines, "\n")
}
