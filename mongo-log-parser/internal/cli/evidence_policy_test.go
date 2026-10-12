package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONEnvelopeCarriesObservationPolicyWithoutRewritingEvidence(t *testing.T) {
	var out bytes.Buffer
	data := map[string]any{"raw": "Cause: missing index", "count": 1}
	if err := emit(&out, true, "test", "case", data, "original table"); err != nil {
		t.Fatal(err)
	}
	var got envelope
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.EvidencePolicy != evidencePolicy || got.Render != "original table" || got.Data.(map[string]any)["raw"] != data["raw"] {
		t.Fatalf("policy or raw evidence lost: %+v", got)
	}
}

func TestTimedRuleReadingDoesNotClaimElapsedSeconds(t *testing.T) {
	for _, r := range ruleCatalogue() {
		if r.Timed() && (!strings.Contains(r.Reading, "valid-sample share") || !strings.Contains(r.Reading, "not continuous elapsed time")) {
			t.Errorf("%s reading conflates samples with continuous duration: %s", r.Name, r.Reading)
		}
	}
}

// Notes must reach JSON consumers even when they do not read the terminal rendering.
func TestJSONEnvelopePreservesUnavailableAndDamageNotes(t *testing.T) {
	var out bytes.Buffer
	notes := []string{"node B: metric unavailable", "node A: damaged FTDC chunk"}
	if err := emit(&out, true, "label", "case", []openedEntry{}, "terminal table", notes); err != nil {
		t.Fatal(err)
	}
	var got envelope
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Notes) != len(notes) {
		t.Fatalf("notes lost: %+v", got)
	}
	for i, note := range notes {
		if got.Notes[i] != note {
			t.Fatalf("note changed: %+v", got)
		}
	}
	if _, ok := got.Data.([]any); !ok {
		t.Fatalf("array data shape changed: %+v", got.Data)
	}
}
