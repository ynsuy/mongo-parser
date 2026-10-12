package analyze

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// A shape is a summary, and a summary cannot be read. "1000 deletes averaging two seconds" says
// nothing about what was being deleted, on which node, or what the slow ones had in common — and
// those are the next three questions anybody asks. This keeps a handful of the real lines.

// Execution is one logged slow operation, kept whole enough to read.
type Execution struct {
	At        time.Time `json:"at"`
	Source    string    `json:"source"`
	Context   string    `json:"context,omitempty"`
	Namespace string    `json:"ns"`
	Operation string    `json:"operation"`
	Plan      string    `json:"plan"`
	Millis    float64   `json:"duration_ms"`

	Docs     *float64 `json:"docs_examined,omitempty"` // nil when the line did not carry it
	Keys     *float64 `json:"keys_examined,omitempty"`
	Returned *float64 `json:"returned,omitempty"`
	Yields   float64  `json:"num_yields"`

	// Waits are the same names the breakdown uses, so one execution can be read against the
	// distribution above it without translating anything.
	Waits map[string]float64 `json:"waits,omitempty"`

	// Command is the command document as the server logged it. It carries the query predicate,
	// which is the single most useful thing on the line and the single most sensitive.
	Command json.RawMessage `json:"command,omitempty"`
	AppName string          `json:"application,omitempty"`
}

// ShapeFilter selects which executions to keep. An empty field matches anything.
type ShapeFilter struct {
	Namespace string
	Operation string
	Plan      string
	Hash      string
}

func (f ShapeFilter) matches(namespace, operation, plan, hash string) bool {
	return (f.Namespace == "" || f.Namespace == namespace) &&
		MatchesOperation(f.Operation, operation) &&
		(f.Plan == "" || f.Plan == plan) &&
		(f.Hash == "" || f.Hash == hash)
}

// operationGroups let a caller ask for a kind of work instead of naming every command that does it.
// "Are the writes slow" is the question people actually have, and answering it otherwise means
// remembering that findAndModify is a write and getMore is a read.
var operationGroups = map[string][]string{
	"writes": {"insert", "update", "delete", "findandmodify", "bulkwrite"},
	"reads":  {"find", "aggregate", "getmore", "count", "distinct"},
	"ddl":    {"createindexes", "dropindexes", "create", "drop", "dropdatabase", "renamecollection", "collmod", "shardcollection"},
}

// MatchesOperation reports whether an operation answers to what the caller asked for: a group name,
// a single command, or a comma-separated list of either. An empty spec matches everything.
func MatchesOperation(spec, operation string) bool {
	if spec == "" {
		return true
	}
	operation = strings.ToLower(operation)
	for _, want := range strings.Split(strings.ToLower(spec), ",") {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		if group, isGroup := operationGroups[want]; isGroup {
			for _, name := range group {
				if name == operation {
					return true
				}
			}
			continue
		}
		if want == operation {
			return true
		}
	}
	return false
}

// OperationGroups names the groups a caller may ask for, for a flag's help text.
func OperationGroups() []string {
	names := make([]string, 0, len(operationGroups))
	for name := range operationGroups {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Executions collects the slowest logged executions of the shapes a filter keeps.
//
// The slowest rather than the first: a sample of the fastest thousand deletes explains nothing
// about the window, and a reader asking to see one is asking to see a bad one.
type Executions struct {
	filter ShapeFilter
	limit  int
	byKey  map[string][]Execution
}

// SampleShapes keeps up to limit executions of every shape the filter admits. It is fed by the
// SlowQueries it is attached to (see SlowQueries.Sample), not by reading records itself.
func SampleShapes(filter ShapeFilter, limit int) *Executions {
	return &Executions{filter: filter, limit: max(limit, 0), byKey: map[string][]Execution{}}
}

// maxSampledShapes bounds how many shapes a sample may cover, so an unfiltered scan of a bundle
// with thousands of shapes cannot grow without limit.
const maxSampledShapes = 200

// offer considers one execution as SlowQueries counted it: a write already folded into the command
// envelope that wrapped it, so the sample carries the duration, waits and command the client saw,
// under the same identity as the shape it is shown beside. Reading raw lines instead would sample
// the statement half of a write, which has neither the client's wait nor its write concern.
func (e *Executions) offer(record Record, attributes slowAttributes) {
	if e == nil || e.limit == 0 {
		return
	}
	operation, collection := operationOf(attributes)
	namespace, plan, hash := namespaceOf(attributes.Namespace, collection), planOf(attributes.PlanSummary), loggedHash(attributes)
	if !e.filter.matches(namespace, operation, plan, hash) {
		return
	}
	key := namespace + "\x00" + operation + "\x00" + plan + "\x00" + hash
	kept, seen := e.byKey[key]
	if !seen && len(e.byKey) >= maxSampledShapes {
		return
	}
	// Cheap rejection before building the Execution: once the bucket is full, only something
	// slower than its fastest member is worth keeping.
	if len(kept) >= e.limit && attributes.DurationMillis <= kept[len(kept)-1].Millis {
		return
	}
	e.byKey[key] = keep(kept, executionOf(record, attributes, namespace, operation, plan), e.limit)
}

func executionOf(record Record, attributes slowAttributes, namespace, operation, plan string) Execution {
	lockWait := 0.0
	for _, lock := range attributes.Locks {
		lockWait += sumModes(lock.TimeAcquiringMicros)
	}
	waits := map[string]float64{
		"flow control":  attributes.FlowControl.TimeAcquiringMicros / 1000,
		"write concern": attributes.WriteConcernWaitMillis,
		"lock acquire":  lockWait / 1000,
		"ticket queue":  attributes.Queues.Execution.TotalTimeQueuedMicros / 1000,
		"storage read":  (attributes.Storage.Data.TimeReadingMicros + attributes.Storage.Data.TimeWaitingMicros) / 1000,
		"remote shards": attributes.RemoteOpWaitMillis,
		"cpu":           attributes.CPUNanos / 1e6,
	}
	if attributes.PlanningTimeMicro != nil {
		waits["planning"] = *attributes.PlanningTimeMicro / 1000
	}
	for name, value := range waits {
		if value <= 0 {
			delete(waits, name)
		}
	}
	return Execution{
		At: record.At, Source: record.Source, Context: record.Ctx,
		Namespace: namespace, Operation: operation, Plan: plan, Millis: attributes.DurationMillis,
		Docs: attributes.DocsExamined, Keys: attributes.KeysExamined, Returned: attributes.NReturned,
		Yields: attributes.NumYields, Waits: waits, Command: attributes.Command, AppName: attributes.AppName,
	}
}

// keep inserts one execution into a slice held sorted slowest first, bounded to limit.
func keep(kept []Execution, entry Execution, limit int) []Execution {
	kept = append(kept, entry)
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Millis > kept[j].Millis })
	if len(kept) > limit {
		kept = kept[:limit]
	}
	return kept
}

// Of returns the sampled executions of one shape, slowest first.
func (e *Executions) Of(namespace, operation, plan, hash string) []Execution {
	return e.byKey[namespace+"\x00"+operation+"\x00"+plan+"\x00"+hash]
}
