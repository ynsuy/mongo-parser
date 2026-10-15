package cli

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/ynsuy/mongo-parser/internal/analyze"
)

func TestSlowExecutionExamplesCarryExactCommandsAndScopeInJSON(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)
	shape := analyze.Shape{ID: "hash", Namespace: "test.events", Operation: "insert", Plan: "", GroupedBy: "queryHash", Count: 12, First: at.Add(-time.Hour), Last: at}
	runs := []analyze.Execution{{At: at, Source: "node-a", Context: "conn42", Namespace: shape.Namespace,
		Operation: shape.Operation, Millis: 7890, Waits: map[string]float64{"write concern": 7800},
		Command: json.RawMessage(`{"insert":"events","ordered":true,"writeConcern":{"j":true,"w":2,"wtimeout":10000}}`)}}
	var out bytes.Buffer
	if err := emit(&out, true, "slow", "case", map[string]any{"execution_examples": slowExecutionExamples(shape, runs, 3)}, "table prefix"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Data struct {
			Examples struct {
				Count      int                 `json:"observed_operations"`
				Requested  int                 `json:"requested_count"`
				Returned   int                 `json:"returned_count"`
				First      time.Time           `json:"first_at"`
				Last       time.Time           `json:"last_at"`
				Sampling   string              `json:"sampling"`
				Executions []analyze.Execution `json:"executions"`
			} `json:"execution_examples"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	examples := got.Data.Examples
	if examples.Count != 12 || examples.Requested != 3 || examples.Returned != 1 || examples.Sampling == "" || !examples.First.Equal(shape.First) || !examples.Last.Equal(shape.Last) {
		t.Fatalf("scope lost: %+v", examples)
	}
	if len(examples.Executions) != 1 {
		t.Fatalf("executions lost: %+v", examples)
	}
	run := examples.Executions[0]
	if !run.At.Equal(at) || run.Source != "node-a" || run.Context != "conn42" || run.Waits["write concern"] != 7800 {
		t.Fatalf("provenance/timings lost: %+v", run)
	}
	var command map[string]any
	if err := json.Unmarshal(run.Command, &command); err != nil {
		t.Fatal(err)
	}
	concern := command["writeConcern"].(map[string]any)
	if concern["w"] != float64(2) || concern["j"] != true || concern["wtimeout"] != float64(10000) {
		t.Fatalf("command changed: %s", run.Command)
	}
}

func TestSlowExecutionExamplesEmptyDoesNotClaimNoObservedOperations(t *testing.T) {
	got := slowExecutionExamples(analyze.Shape{Count: 12}, nil, 0)
	if got["observed_operations"] != 12 || got["returned_count"] != 0 || got["executions"] == nil {
		t.Fatalf("empty sample conflates absence: %+v", got)
	}
}
