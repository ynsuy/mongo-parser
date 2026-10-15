package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/ynsuy/mongo-parser/internal/label"
)

// runLabels prints every label the rules can put on a bundle: what it measures, from which FTDC
// metrics, which summary of each minute it is read on, for which server versions, and the bands it
// is read against. It takes no bundle; it is the rule book, for whoever is reviewing the thresholds.
func runLabels(_ string, args []string) error {
	fs, c := newFlags("labels")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	return emit(os.Stdout, c.json, "labels", "", ruleCatalogue(), withVersion(labelText()))
}

// ruleEntry is one definition with the parts a reader would otherwise work out spelled out.
type ruleEntry struct {
	label.Rule
	Unit     string `json:"unit"`
	Formula  string `json:"formula"`
	Reading  string `json:"reading"`
	Bands    string `json:"bands"`
	Versions string `json:"version_text"`
}

// ruleCatalogue is what every label means, for whoever reads the JSON or reviews the rules.
func ruleCatalogue() []ruleEntry {
	out := make([]ruleEntry, 0, len(label.Rules))
	for _, r := range label.Rules {
		out = append(out, catalogueEntry(r))
	}
	return out
}

func catalogueEntry(r label.Rule) ruleEntry {
	return ruleEntry{Rule: r, Unit: r.Unit(), Formula: formula(r), Reading: readOn(r),
		Bands: levelText(r.Levels), Versions: r.VersionText()}
}

func labelText() string {
	lines := make([]string, 0, len(label.Rules)*7)
	for _, r := range label.Rules {
		lines = append(lines, fmt.Sprintf("%s — %s [%s] (%s)", r.Name, r.Title, r.VersionText(), r.Unit()))
		lines = append(lines, "  signal:  "+formula(r))
		lines = append(lines, "  read as: "+readOn(r))
		lines = append(lines, "  levels:  "+levelText(r.Levels))
		if r.Elevated != nil {
			lines = append(lines, fmt.Sprintf("  mark:    %g %s (the duty cycle counts the minutes at or past it)", *r.Elevated, r.Unit()))
		}
		lines = append(lines, fmt.Sprintf("  change:  %g %s is the smallest move that counts", r.MinChange, r.Unit()))
		if ref := referenceText(r); ref != "" {
			lines = append(lines, "  ref:     "+ref)
		}
		if r.Applies != "" {
			lines = append(lines, "  applies: "+strings.Join(strings.Fields(r.Applies), " "))
		}
		if len(r.PairsWith) > 0 {
			lines = append(lines, "  pairs:   "+strings.Join(r.PairsWith, ", "))
		}
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// referenceText names the server default a rule's fixed number comes from, or "".
func referenceText(r label.Rule) string {
	if r.Reference == nil {
		return ""
	}
	return fmt.Sprintf("%g %s is %s; the configured value is %s", r.Reference.Value, r.Reference.Unit, r.Reference.Source, r.Reference.Configured)
}

// formula is how one second's value is computed, as a person would write it.
func formula(r label.Rule) string {
	s := r.Signal
	sum := func(paths []string) string {
		if len(paths) == 1 {
			return paths[0]
		}
		return "(" + strings.Join(paths, " + ") + ")"
	}
	text := sum(s.Num)
	if s.Mode == "rate" {
		text = "Δ" + text
	}
	if len(s.Minus) > 0 {
		text += " − " + sum(s.Minus)
	}
	if s.Behind {
		text = "newest of every " + s.Num[0] + " − this one"
	}
	if len(s.Den) > 0 {
		den := sum(s.Den)
		if s.Mode == "rate" {
			den = "Δ" + den
		}
		text += " ÷ " + den
	}
	if s.Mode == "rate" && len(s.Den) == 0 {
		text += " per second"
	}
	if s.Scale != 1 && s.Scale != 0 {
		text += fmt.Sprintf(" × %g", s.Scale)
	}
	if s.Complement {
		text = fmt.Sprintf("%g − (%s)", s.Scale, text)
	}
	text += ", in " + s.Unit + ", at each available sample"
	if s.MinDen > 0 {
		text += fmt.Sprintf("; minutes where the divisor sums to under %g are left out", s.MinDen)
	}
	if s.RealDevice {
		text += "; real block devices only"
	}
	return text
}

// readOn is which summary of each minute the bands and the shape are read on.
func readOn(r label.Rule) string {
	switch r.View {
	case label.ViewMax:
		return "the highest valid sample of each minute"
	case label.ViewMin:
		return "the lowest valid sample of each minute"
	case label.ViewTimeAbove:
		return fmt.Sprintf("valid-sample share strictly above %g %s, normalized to 60 s/min; not continuous elapsed time", *r.Threshold, r.Signal.Unit)
	case label.ViewTimeBelow:
		return fmt.Sprintf("valid-sample share strictly below %g %s, normalized to 60 s/min; not continuous elapsed time", *r.Threshold, r.Signal.Unit)
	}
	if len(r.Signal.Den) > 0 {
		return "each minute's ratio of sums"
	}
	return "each minute's mean"
}

func levelText(levels []label.Level) string {
	if len(levels) == 0 {
		return "none — read for its shape only"
	}
	parts := make([]string, 0, len(levels))
	for _, l := range levels {
		switch {
		case len(l.Down) == 2:
			parts = append(parts, fmt.Sprintf("%s ≤%g (fading to %g)", l.Name, l.Down[0], l.Down[1]))
		case len(l.Up) == 2:
			parts = append(parts, fmt.Sprintf("%s ≥%g (from %g)", l.Name, l.Up[1], l.Up[0]))
		case len(l.Band) == 4:
			parts = append(parts, fmt.Sprintf("%s %g–%g (edges %g, %g)", l.Name, l.Band[1], l.Band[2], l.Band[0], l.Band[3]))
		}
	}
	return strings.Join(parts, " · ")
}
