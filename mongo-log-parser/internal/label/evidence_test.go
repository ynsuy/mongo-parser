package label

import (
	"strings"
	"testing"
)

// This is a regression guard for previously diagnosed causes, not a semantic proof of neutrality.
// New wording still needs review; customer log text is intentionally not filtered.
func TestEveryRuleDescribesEvidenceWithoutPresetDiagnoses(t *testing.T) {
	for _, r := range Rules {
		doc := strings.ToLower(strings.Join(strings.Fields(r.Doc), " "))
		for _, phrase := range []string{
			"means hot documents", "a missing index", "a classic periodic cause", "the disk is behind",
			"an applier that is stuck", "every operation's latency grows", "each one is latency added",
			"writes slow down in exactly", "usually short of disk", "a client leaking connections",
			"because the eviction workers", "is at its bandwidth limit",
		} {
			if strings.Contains(doc, phrase) {
				t.Errorf("%s presets a cause: %q", r.Name, phrase)
			}
		}
		// The reading policy travels once, in every JSON envelope and in each rule's "read as"; a
		// copy in every doc only buries what is particular to the rule.
		for _, boilerplate := range []string{"fixed descriptive references", "not causal claims", "ratio of sums for ratio signals"} {
			if strings.Contains(doc, boilerplate) {
				t.Errorf("%s repeats the shared reading policy (%q) instead of saying what it measures", r.Name, boilerplate)
			}
		}
		// A time view says what was past its threshold, not "the signal".
		if r.Timed() && strings.Contains(doc, "the signal strictly") {
			t.Errorf("%s does not name what it counts: %s", r.Name, r.Doc)
		}
		for _, l := range r.Levels {
			if word := diagnosticWord(l.Name); word != "" {
				t.Errorf("%s carries a diagnostic band name: %s", r.Name, l.Name)
			}
		}
		// The ID and the title are what a reader quotes; they name the measurement, not a condition.
		for what, text := range map[string]string{"ID": r.Name, "title": r.Title} {
			if word := diagnosticWord(text); word != "" {
				t.Errorf("%s %s %q names a condition (%s) rather than the measurement", r.Name, what, text, word)
			}
		}
	}
}

// diagnosticWord returns the first word in text that asserts a condition the numbers alone do not
// establish, or "".
func diagnosticWord(text string) string {
	lower := strings.ToLower(text)
	for _, word := range []string{
		"saturat", "over_trigger", "exhausted", "overloaded", "lossy", "severe", "scanning", "very_poor",
		"excessive", "piled_up", "throttl", "idle connections", "in_memory", "storm", "bottleneck", "stall",
	} {
		if strings.Contains(lower, word) {
			return word
		}
	}
	return ""
}

// A fixed number that is a server default says so, and says the node's own setting was not read;
// a signal that only means something in one role or setting says which.
func TestDefaultsAndApplicabilityTravelWithTheRule(t *testing.T) {
	for _, name := range []string{"cache.fill", "cache.fill_above_95", "cache.dirty", "cache.dirty_above_20", "checkpoint.duration"} {
		for _, r := range Rules {
			if r.Name != name {
				continue
			}
			if r.Reference == nil || r.Reference.Source == "" || r.Reference.Configured != "not read" {
				t.Errorf("%s: reference %+v, want a named default and an unread configured value", name, r.Reference)
			}
		}
	}
	for _, name := range []string{"flow.lagged", "repl.member_lag", "disk.busy", "tickets.read.used", "tickets.write.used"} {
		for _, r := range Rules {
			if r.Name == name && r.Applies == "" {
				t.Errorf("%s does not say when it applies", name)
			}
		}
	}
}
