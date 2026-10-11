package label

import (
	"strings"
	"testing"
)

// This is a regression guard for previously diagnosed causes, not a semantic proof of neutrality.
// New wording still needs review; customer log text is intentionally not filtered.
func TestEveryRuleDescribesEvidenceWithoutPresetDiagnoses(t *testing.T) {
	for _, r := range Rules {
		if !strings.Contains(r.Doc, "fixed descriptive references") || !strings.Contains(r.Doc, "not causal claims") {
			t.Errorf("%s lacks observation/reference scope", r.Name)
		}
		for _, phrase := range []string{
			"means hot documents", "a missing index", "a classic periodic cause", "the disk is behind",
			"an applier that is stuck", "every operation's latency grows", "each one is latency added",
			"writes slow down in exactly", "usually short of disk", "a client leaking connections",
			"because the eviction workers", "is at its bandwidth limit",
		} {
			if strings.Contains(strings.ToLower(r.Doc), phrase) {
				t.Errorf("%s presets a cause: %q", r.Name, phrase)
			}
		}
		if r.Timed() && !strings.Contains(r.Doc, "not measured continuous elapsed duration") {
			t.Errorf("%s presents normalized samples as continuous duration", r.Name)
		}
		for _, l := range r.Levels {
			switch l.Name {
			case "saturated", "over_trigger", "exhausted", "overloaded", "lossy", "severe", "scanning", "very_poor", "excessive", "piled_up":
				t.Errorf("%s carries a diagnostic band name: %s", r.Name, l.Name)
			}
		}
	}
}
