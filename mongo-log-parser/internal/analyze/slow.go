package analyze

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// slowAttributes are the fields MongoDB writes on a "Slow query" line that say what the operation
// cost. Decoding this typed shape is far cheaper than decoding every attribute into a map.
type slowAttributes struct {
	Type           string  `json:"type"`
	Namespace      string  `json:"ns"`
	QueryHash      string  `json:"queryHash"`
	PlanCacheShape string  `json:"planCacheShapeHash"`
	PlanSummary    string  `json:"planSummary"`
	DurationMillis float64 `json:"durationMillis"`
	// Not every slow line carries these: the command envelope of a write logs none of them, and a
	// field that was not written is unknown, not zero. A count of zero documents examined that the
	// server never reported reads as a perfect index.
	KeysExamined      *float64        `json:"keysExamined"`
	DocsExamined      *float64        `json:"docsExamined"`
	NReturned         *float64        `json:"nreturned"`
	ResponseLength    float64         `json:"reslen"`
	NumYields         float64         `json:"numYields"`
	PlanningTimeMicro *float64        `json:"planningTimeMicros"`
	CPUNanos          float64         `json:"cpuNanos"`
	AppName           string          `json:"appName"`
	Command           json.RawMessage `json:"command"`

	// The outcome. A slow line says how long the operation took; these three say whether it
	// achieved anything. When a shard cannot read its routing table, every query burns its whole
	// maxTimeMS and fails, and a table that reports only the duration calls that
	// a slow query when it is a broken one.
	OK      *float64 `json:"ok"`
	ErrName string   `json:"errName"`
	ErrCode int      `json:"errCode"`
	Storage struct {
		Data struct {
			BytesRead         float64 `json:"bytesRead"`
			TimeReadingMicros float64 `json:"timeReadingMicros"`
			TimeWaitingMicros float64 `json:"timeWaitingMicros"`
		} `json:"data"`
	} `json:"storage"`

	// The fields below are what the duration was actually spent on. An operation that took five
	// seconds on an idle machine spent them waiting for something, and the log says which thing —
	// in attributes this tool used to decode and throw away.
	FlowControl struct {
		AcquireCount        float64 `json:"acquireCount"`
		TimeAcquiringMicros float64 `json:"timeAcquiringMicros"`
	} `json:"flowControl"`
	Locks                  map[string]lockAttributes `json:"locks"`
	WriteConcernWaitMillis float64                   `json:"waitForWriteConcernDurationMillis"`
	RemoteOpWaitMillis     float64                   `json:"remoteOpWaitMillis"`
	WorkingMillis          float64                   `json:"workingMillis"`
	Queues                 struct {
		Execution struct {
			Admissions            float64 `json:"admissions"`
			TotalTimeQueuedMicros float64 `json:"totalTimeQueuedMicros"`
		} `json:"execution"`
	} `json:"queues"`
}

// lockAttributes is one lock's line in the locks document. Each figure is keyed by the mode the
// lock was taken in — r, w, R, W — so the wait is a sum over the modes and not a single number.
type lockAttributes struct {
	AcquireCount        map[string]float64 `json:"acquireCount"`
	AcquireWaitCount    map[string]float64 `json:"acquireWaitCount"`
	TimeAcquiringMicros map[string]float64 `json:"timeAcquiringMicros"`
}

func sumModes(byMode map[string]float64) float64 {
	total := 0.0
	for _, value := range byMode {
		total += value
	}
	return total
}

// Shape is one family of slow queries: same namespace, operation and plan, counted together.
// MongoDB's logged hash groups them when present; hashless rows are namespace/operation/plan groups.
type Shape struct {
	ID        string    `json:"shape_id"`
	Namespace string    `json:"ns"`
	Operation string    `json:"operation"`
	Plan      string    `json:"plan"`
	Count     int       `json:"count"`
	First     time.Time `json:"first_at,omitzero"`
	Last      time.Time `json:"last_at,omitzero"`

	// Keys, Docs, Returned and Planning count only the executions whose line carried the field, so
	// their count can be below the shape's; one that no line carried is left out of the JSON.
	Duration  Numbers `json:"duration_ms"`
	Keys      Numbers `json:"keys_examined,omitzero"`
	Docs      Numbers `json:"docs_examined,omitzero"`
	Returned  Numbers `json:"returned,omitzero"`
	BytesRead Numbers `json:"bytes_read"`
	Planning  Numbers `json:"planning_micros,omitzero"`

	// The waits, all in milliseconds so they can be read against Duration. An operation that is
	// slow because it waited and one that is slow because it did work print identically without
	// these, and the two have nothing in common but the number.
	FlowControlWait  Numbers `json:"flow_control_wait_ms"`
	LockWait         Numbers `json:"lock_wait_ms"`
	WriteConcernWait Numbers `json:"write_concern_wait_ms"`
	RemoteWait       Numbers `json:"remote_wait_ms"`
	StorageWait      Numbers `json:"storage_wait_ms"`
	QueueWait        Numbers `json:"queue_wait_ms"`
	CPU              Numbers `json:"cpu_ms"`
	LockAcquireWaits Numbers `json:"lock_acquire_waits"`

	// Timeline is when this shape ran. The table says a shape cost four minutes of server time
	// over two days; it cannot say whether that was a steady drip or one bad ten minutes, and those
	// are different problems with different fixes.
	Timeline Timeline `json:"timeline,omitempty"`

	// Failed counts the executions the server said did not succeed, and Errors names them. Kept
	// alongside the successful ones rather than split into their own shape: the same query failing
	// and succeeding in one window is the thing worth seeing, and two rows hide it.
	Failed       int            `json:"failed"`
	FailedMillis float64        `json:"failed_ms"`
	Errors       map[string]int `json:"errors,omitempty"`

	Sources            map[string]int `json:"sources"`
	Apps               map[string]int `json:"applications,omitempty"`
	GroupedBy          string         `json:"grouped_by"`
	QueryHash          string         `json:"query_hash,omitempty"`
	PlanCacheShapeHash string         `json:"plan_cache_shape_hash,omitempty"`
}

// Inefficiency is how many documents were examined per document returned. A scan that reads
// thousands to return one is the classic index problem.
//
// It is a reading operation's measure and nothing else. A write returns no documents, so the ratio
// is its whole examined count over one: an update that found its document by _id and touched
// exactly one row would read as thousands of documents read per document returned, which looks
// like an index problem and is not. Nothing can be read off a write's nreturned, and saying so is
// better than inventing a number from it.
func (s Shape) Inefficiency() float64 {
	if !s.Reads() || s.Docs.Count == 0 || s.Returned.Count == 0 {
		return 0
	}
	return s.Docs.Sum / (s.Returned.Sum + 1)
}

// Reads reports whether this shape is an operation whose nreturned means anything.
func (s Shape) Reads() bool {
	switch s.Operation {
	case "find", "aggregate", "getMore", "count", "distinct", "query", "listIndexes", "listCollections":
		return true
	}
	return false
}

// TotalTime is what this shape cost the server over the window, which is what makes a shape worth
// looking at before a rarer but individually slower one.
func (s Shape) TotalTime() float64 { return s.Duration.Sum }

// TimelineStep is the resolution a shape's history is kept at. A minute is fine enough to re-bin to
// whatever a chart asks for and coarse enough that three days of it is a few thousand counters —
// and it is sparse, so a shape that ran twice costs two.
const TimelineStep = 60

// Bin is one step of that history: how many ran, what they cost between them, and the fastest and
// slowest of them.
//
// The bounds are kept because an average over a bin hides the thing worth seeing. A minute whose
// mean is 400 ms can be four hundred operations at 400 ms or three hundred and ninety-nine at 50 ms
// and one at two minutes, and those are different problems. Two floats per minute is cheap, and the
// history is sparse.
type Bin struct {
	Count  int     `json:"count"`
	Millis float64 `json:"millis"`
	Min    float64 `json:"min_ms"`
	Max    float64 `json:"max_ms"`
	// Wait is how much of Millis the server said was spent waiting rather than working. One number
	// rather than one per kind: seven timelines per shape is megabytes on a bundle with thousands
	// of shapes, and the question a chart answers here is whether the waiting tracks the slowness,
	// which the totals table then names.
	Wait float64 `json:"wait_ms"`
}

// Avg is what one execution cost in this bin.
func (b Bin) Avg() float64 {
	if b.Count == 0 {
		return 0
	}
	return b.Millis / float64(b.Count)
}

// AvgWait is how much of that one execution was spent waiting.
func (b Bin) AvgWait() float64 {
	if b.Count == 0 {
		return 0
	}
	return b.Wait / float64(b.Count)
}

// Timeline is a shape's history, keyed by the Unix second its step began. A step with nothing in it
// is absent rather than zero; whoever draws it decides what the gaps mean.
type Timeline map[int64]Bin

// Add counts one execution: what it cost, and how much of that it spent waiting.
func (t Timeline) Add(at time.Time, millis, wait float64) {
	step := at.Unix() / TimelineStep * TimelineStep
	bin := t[step]
	if bin.Count == 0 || millis < bin.Min {
		bin.Min = millis
	}
	if millis > bin.Max {
		bin.Max = millis
	}
	bin.Count, bin.Millis, bin.Wait = bin.Count+1, bin.Millis+millis, bin.Wait+wait
	t[step] = bin
}

// Merge folds another history in.
func (t Timeline) Merge(other Timeline) {
	for step, bin := range other {
		t[step] = combine(t[step], bin)
	}
}

// combine adds one bin into another. The bounds are bounds and not sums, and an empty destination
// has no minimum to compare against — zero would win every comparison and report every bin as
// starting at nothing.
func combine(into, bin Bin) Bin {
	if bin.Count == 0 {
		return into
	}
	if into.Count == 0 || bin.Min < into.Min {
		into.Min = bin.Min
	}
	if bin.Max > into.Max {
		into.Max = bin.Max
	}
	into.Count, into.Millis, into.Wait = into.Count+bin.Count, into.Millis+bin.Millis, into.Wait+bin.Wait
	return into
}

// Fold rebins a history into coarser steps, adding it to what is already in the destination. It is
// how several shapes become the one line a chart draws.
func (t Timeline) Fold(seconds int64, into Timeline) {
	if seconds <= 0 {
		seconds = TimelineStep
	}
	for step, bin := range t {
		key := step / seconds * seconds
		into[key] = combine(into[key], bin)
	}
}

// Numbers summarizes a value across a shape without keeping every sample: counts, bounds and sum
// are exact, and the percentile is read off a log-scale histogram.
type Numbers struct {
	Count     int     `json:"count"`
	Sum       float64 `json:"sum"`
	Min       float64 `json:"min"`
	Max       float64 `json:"max"`
	histogram [histogramBuckets]int
}

// histogramBuckets covers 1 to ~10^7 in tenth-of-a-decade steps, which is enough resolution for
// a latency percentile and costs a fixed 70 counters per value.
const histogramBuckets = 70

// IsZero reports whether nothing was counted, so a measure no line carried is left out of the JSON
// rather than written as a measured zero.
func (n Numbers) IsZero() bool { return n.Count == 0 }

// addLogged counts a value the line may not have carried.
func (n *Numbers) addLogged(value *float64) {
	if value != nil {
		n.add(*value)
	}
}

func (n *Numbers) add(value float64) {
	if n.Count == 0 || value < n.Min {
		n.Min = value
	}
	if value > n.Max {
		n.Max = value
	}
	n.Count++
	n.Sum += value
	n.histogram[bucketOf(value)]++
}

func bucketOf(value float64) int {
	if value <= 1 {
		return 0
	}
	bucket := int(math.Log10(value) * 10)
	return min(max(bucket, 0), histogramBuckets-1)
}

// Avg is the mean of the values seen.
func (n Numbers) Avg() float64 {
	if n.Count == 0 {
		return 0
	}
	return n.Sum / float64(n.Count)
}

// Quantile is approximate: it names the bucket the value falls in, bounded by the exact min and max.
//
// The first bucket holds everything at or below 1, which is where zeros live, and it has no
// resolution inside it at all. Reading it as the bucket's own lower edge reported 1 for a
// distribution of zeros — a median of "1 byte read" on a shape that read nothing, and a p50 above
// the mean, which no distribution can do. The honest answer for that bucket is the exact minimum:
// the true value lies between it and 1, and the minimum is measured rather than assumed.
func (n Numbers) Quantile(fraction float64) float64 {
	if n.Count == 0 {
		return 0
	}
	target, seen := int(math.Ceil(float64(n.Count)*fraction)), 0
	for bucket, count := range n.histogram {
		if seen += count; seen >= target {
			if bucket == 0 {
				return n.Min
			}
			return min(max(math.Pow(10, float64(bucket)/10), n.Min), n.Max)
		}
	}
	return n.Max
}

// SlowQueries folds "Slow query" lines into shapes.
type SlowQueries struct {
	shapes map[string]*Shape
	// pending holds write statements waiting for the command envelope that wraps them, keyed by the
	// source and log context — connection names are only unique within a node. See pendingWrite.
	pending map[string]pendingWrite
	// sample, when attached, is offered every execution as it is counted.
	sample *Executions
}

// Sample attaches a sampler: every execution counted from here on is offered to it after write
// pairing. Its samples are complete once Shapes or another aggregate has flushed the pairing.
func (s *SlowQueries) Sample(e *Executions) { s.sample = e }

func NewSlowQueries() *SlowQueries {
	return &SlowQueries{shapes: map[string]*Shape{}, pending: map[string]pendingWrite{}}
}

// pendingWrite is one per-statement write line held back until its command envelope arrives.
//
// mongod logs a slow write twice: once as the statement it executed, with the plan and the
// documents it touched, and once as the command the client sent, with the total the client waited
// and the write concern inside it. Both name the same collection once "db.$cmd" is resolved, so
// counting both reports one update as two, and a collection's total as twice what the server
// actually spent. The pair
// arrives on the same connection in the same millisecond, so holding the statement back until the
// next line on that connection is enough to recognise it.
type pendingWrite struct {
	record     Record
	attributes slowAttributes
	namespace  string
}

// pendingLimit bounds the memo when a log's envelopes are missing entirely — a truncated file, or a
// server that logged the statement and was killed before the command finished. Beyond it the oldest
// entries are simply counted as they stand, which is what happened before any of this existed.
const pendingLimit = 1 << 16

// Add counts one record; anything that is not a slow query is ignored.
func (s *SlowQueries) Add(record Record) {
	if record.Msg != "Slow query" {
		return
	}
	var attributes slowAttributes
	if !record.DecodeAttributes(&attributes) {
		return
	}
	s.offer(record, attributes)
}

// offer routes one decoded line through the envelope pairing before it becomes a shape.
func (s *SlowQueries) offer(record Record, attributes slowAttributes) {
	key := record.Source + "\x00" + record.Ctx
	if held, waiting := s.pending[key]; waiting {
		delete(s.pending, key)
		if wraps(held, record, attributes) {
			s.count(record, wrapped(held.attributes, attributes))
			return
		}
		s.count(held.record, held.attributes)
	}
	if namespace, ok := writeStatement(attributes); ok {
		if len(s.pending) < pendingLimit {
			s.pending[key] = pendingWrite{record: record, attributes: attributes, namespace: namespace}
			return
		}
	}
	s.count(record, attributes)
}

// writeStatement reports whether a line is the per-statement half of a write, and against what.
// The statement names its collection outright; the envelope reaches the same name through "$cmd".
func writeStatement(attributes slowAttributes) (string, bool) {
	if attributes.Type != "update" && attributes.Type != "remove" {
		return "", false
	}
	if attributes.Namespace == "" || strings.HasSuffix(attributes.Namespace, ".$cmd") {
		return "", false
	}
	return attributes.Namespace, true
}

// wraps reports whether this line is the command envelope of the statement being held: the same
// connection, the same collection, and close enough in time to be the same operation.
func wraps(held pendingWrite, record Record, attributes slowAttributes) bool {
	if held.record.Source != record.Source || held.record.Ctx != record.Ctx {
		return false
	}
	if attributes.Type != "command" || !strings.HasSuffix(attributes.Namespace, ".$cmd") {
		return false
	}
	_, collection := operationOf(attributes)
	if namespaceOf(attributes.Namespace, collection) != held.namespace {
		return false
	}
	gap := record.At.Sub(held.record.At)
	return gap >= -pairingWindow && gap <= pairingWindow
}

// pairingWindow is how far apart the two halves of one write may be logged. They are written one
// after the other by the same thread, so this is slack for clock granularity, not for searching.
const pairingWindow = time.Second

// wrapped folds a statement into the envelope that wrapped it. The envelope is what the client
// waited for, so its duration and its waits win; the statement is the only half that says which
// plan ran and what it read, so those fields come from it when the envelope has none.
func wrapped(statement, envelope slowAttributes) slowAttributes {
	merged := envelope
	merged.Namespace = statement.Namespace
	if merged.PlanSummary == "" {
		merged.PlanSummary = statement.PlanSummary
	}
	if merged.QueryHash == "" {
		merged.QueryHash = statement.QueryHash
	}
	if merged.PlanCacheShape == "" {
		merged.PlanCacheShape = statement.PlanCacheShape
	}
	for _, field := range []struct{ into, from *float64 }{
		{&merged.Storage.Data.BytesRead, &statement.Storage.Data.BytesRead},
		{&merged.Storage.Data.TimeReadingMicros, &statement.Storage.Data.TimeReadingMicros},
	} {
		if *field.into == 0 {
			*field.into = *field.from
		}
	}
	for _, field := range []struct{ into, from **float64 }{
		{&merged.KeysExamined, &statement.KeysExamined},
		{&merged.DocsExamined, &statement.DocsExamined},
		{&merged.NReturned, &statement.NReturned},
		{&merged.PlanningTimeMicro, &statement.PlanningTimeMicro},
	} {
		if *field.into == nil || (**field.into == 0 && *field.from != nil) {
			*field.into = *field.from
		}
	}
	return merged
}

// flush counts every statement still waiting for an envelope that never came. Every aggregate this
// type reports calls it first, so nothing is lost by holding a line back.
func (s *SlowQueries) flush() {
	for context, held := range s.pending {
		delete(s.pending, context)
		s.count(held.record, held.attributes)
	}
}

// count folds one line, already paired if it had a pair, into its shape.
func (s *SlowQueries) count(record Record, attributes slowAttributes) {
	s.sample.offer(record, attributes)
	operation, collection := operationOf(attributes)
	namespace := namespaceOf(attributes.Namespace, collection)
	plan := planOf(attributes.PlanSummary)
	key, groupedBy := shapeID(attributes, namespace, operation, plan)
	shape, seen := s.shapes[key]
	if !seen {
		shape = &Shape{
			ID: loggedHash(attributes), Namespace: namespace, Operation: operation, Plan: plan, GroupedBy: groupedBy,
			QueryHash: attributes.QueryHash, PlanCacheShapeHash: attributes.PlanCacheShape, First: record.At,
			Sources: map[string]int{}, Apps: map[string]int{}, Errors: map[string]int{}, Timeline: Timeline{},
		}
		s.shapes[key] = shape
	}
	shape.Count++
	shape.Sources[record.Source]++
	if name, failed := failureOf(attributes); failed {
		shape.Failed++
		shape.FailedMillis += attributes.DurationMillis
		shape.Errors[name]++
	}
	if attributes.AppName != "" {
		shape.Apps[attributes.AppName]++
	}
	if record.At.Before(shape.First) {
		shape.First = record.At
	}
	if record.At.After(shape.Last) {
		shape.Last = record.At
	}
	shape.Duration.add(attributes.DurationMillis)
	shape.Keys.addLogged(attributes.KeysExamined)
	shape.Docs.addLogged(attributes.DocsExamined)
	shape.Returned.addLogged(attributes.NReturned)
	shape.BytesRead.add(attributes.Storage.Data.BytesRead)
	shape.Planning.addLogged(attributes.PlanningTimeMicro)

	lockWait, lockWaits := 0.0, 0.0
	for _, lock := range attributes.Locks {
		lockWait += sumModes(lock.TimeAcquiringMicros)
		lockWaits += sumModes(lock.AcquireWaitCount)
	}
	flowControlWait := attributes.FlowControl.TimeAcquiringMicros / 1000
	storageWait := (attributes.Storage.Data.TimeReadingMicros + attributes.Storage.Data.TimeWaitingMicros) / 1000
	queueWait := attributes.Queues.Execution.TotalTimeQueuedMicros / 1000
	// The timeline carries the waits added together. cpu is work and not a wait, so it is left out
	// of the sum the same way it is left out of the breakdown.
	shape.Timeline.Add(record.At, attributes.DurationMillis,
		flowControlWait+lockWait/1000+queueWait+storageWait+attributes.WriteConcernWaitMillis+attributes.RemoteOpWaitMillis)
	shape.FlowControlWait.add(flowControlWait)
	shape.LockWait.add(lockWait / 1000)
	shape.LockAcquireWaits.add(lockWaits)
	shape.WriteConcernWait.add(attributes.WriteConcernWaitMillis)
	shape.RemoteWait.add(attributes.RemoteOpWaitMillis)
	shape.StorageWait.add(storageWait)
	shape.QueueWait.add(queueWait)
	shape.CPU.add(attributes.CPUNanos / 1e6)
}

// Wait is one thing an operation spent its time on, and what it cost this shape over the window.
type Wait struct {
	Name   string  `json:"name"`
	Millis float64 `json:"millis"`
	// Share is the fraction of the shape's total duration this wait accounts for.
	Share float64 `json:"share"`
	// Next is the command to run when this is the wait that dominates, short enough for a column.
	Next string `json:"next,omitempty"`
	// Meaning is what it means that the time went here, for the one line printed under the table.
	Meaning string `json:"meaning,omitempty"`
}

// waitNames orders the waits the way a reader works through them: the ones outside the operation
// first, then the ones inside it, then what it actually spent on the CPU.
//
// The figures come from different parts of the log line and overlap slightly — a lock taken while
// flow control is being acquired is counted in both — so they are a breakdown and not a partition.
// They are still the difference between "this query is slow" and "this query waited four seconds
// for write concern on a lagging secondary", which is a different problem with a different fix.
func (s Shape) Waits() []Wait {
	total := s.Duration.Sum
	if total <= 0 {
		return nil
	}
	candidates := []Wait{
		{Name: "flow control", Millis: s.FlowControlWait.Sum, Next: "metrics --metric 'flowControl/*'",
			Meaning: "sum of logged flowControl timeAcquiringMicros, converted to milliseconds"},
		{Name: "write concern", Millis: s.WriteConcernWait.Sum, Next: "metrics --metric 'repl/*'",
			Meaning: "sum of logged waitForWriteConcernDurationMillis"},
		{Name: "lock acquire", Millis: s.LockWait.Sum, Next: "metrics --metric 'globalLock/*'",
			Meaning: "sum of logged lock timeAcquiringMicros, converted to milliseconds"},
		{Name: "ticket queue", Millis: s.QueueWait.Sum, Next: "metrics --metric 'concurrentTransactions/*' --metric 'queues/execution/*'",
			Meaning: "sum of logged queues execution totalTimeQueuedMicros, converted to milliseconds"},
		{Name: "storage read", Millis: s.StorageWait.Sum, Next: "metrics --metric 'wiredTiger/cache/*'",
			Meaning: "sum of logged storage data timeReadingMicros and timeWaitingMicros, converted to milliseconds"},
		{Name: "remote shards", Millis: s.RemoteWait.Sum, Next: "slow on the shard's own logs",
			Meaning: "sum of logged remoteOpWaitMillis; no shard identity inferred"},
		// Planning is work rather than waiting, but it is a stretch of the operation's own time the
		// server measured, and left out it was counted as unaccounted.
		{Name: "planning", Millis: s.Planning.Sum / 1000, Next: "-",
			Meaning: "sum of logged planningTimeMicros, converted to milliseconds"},
		{Name: "cpu", Millis: s.CPU.Sum, Next: "-",
			Meaning: "sum of logged cpuNanos, converted to milliseconds"},
	}
	waits := make([]Wait, 0, len(candidates)+1)
	accounted := 0.0
	for _, wait := range candidates {
		if wait.Millis <= 0 {
			continue
		}
		// CPU is work and not a wait, so it does not count against what is left unexplained.
		if wait.Name != "cpu" {
			accounted += wait.Millis
		}
		wait.Share = wait.Millis / total
		waits = append(waits, wait)
	}
	if remainder := total - accounted; remainder > 0 && len(waits) > 0 {
		// This row had no command beside it, which made the table asymmetric in a way that reads
		// as an answer: every named wait offered somewhere to go and the largest unnamed one did
		// not. It is also where the time goes when the server itself stopped serving for a moment,
		// so it is the row to follow when nothing named accounts for the slowness.
		unaccounted := UnaccountedWait()
		unaccounted.Millis, unaccounted.Share = remainder, remainder/total
		waits = append(waits, unaccounted)
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i].Millis > waits[j].Millis })
	return waits
}

// UnaccountedWait is the row for the time no named wait accounts for, before its figures are set.
func UnaccountedWait() Wait {
	return Wait{Name: "unaccounted", Next: "report --only labels,activity",
		Meaning: "positive residual of logged duration minus summed named wait fields; not a measured activity or a cause"}
}

// Waited reports whether this shape spent a meaningful part of its time on something other than
// work, which is what decides whether a breakdown is worth printing at all.
func (s Shape) Waited() bool {
	return s.FlowControlWait.Sum > 0 || s.LockWait.Sum > 0 || s.WriteConcernWait.Sum > 0 ||
		s.RemoteWait.Sum > 0 || s.StorageWait.Sum > 0 || s.QueueWait.Sum > 0
}

// Shapes returns the shapes that cost the server the most time over the window.
func (s *SlowQueries) Shapes() []Shape {
	s.flush()
	out := make([]Shape, 0, len(s.shapes))
	for _, shape := range s.shapes {
		out = append(out, *shape)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTime() != out[j].TotalTime() {
			return out[i].TotalTime() > out[j].TotalTime()
		}
		return out[i].Namespace+"\x00"+out[i].Operation+"\x00"+out[i].Plan+"\x00"+out[i].ID < out[j].Namespace+"\x00"+out[j].Operation+"\x00"+out[j].Plan+"\x00"+out[j].ID
	})
	return out
}

// GroupingCounts says how many shapes were identified each way, so a surprising number of shapes
// can be read for what it is: a server that reported no queryHash, not a server running thousands
// of different queries.
func (s *SlowQueries) GroupingCounts() map[string]int {
	s.flush()
	counts := map[string]int{}
	for _, shape := range s.shapes {
		counts[shape.GroupedBy]++
	}
	return counts
}

// TotalTime is what every logged operation cost the server over the window, whatever became of it.
// It is the denominator the failed total is read against, and it is the bundle's rather than a
// filtered set's: "how much of this workload failed" is a question about the workload.
func (s *SlowQueries) TotalTime() float64 {
	s.flush()
	total := 0.0
	for _, shape := range s.shapes {
		total += shape.Duration.Sum
	}
	return total
}

// WaitTotals is where the time of every shape went, added up across the window. It is the answer to
// "the writes are slow and the machine is idle": the total duration on one side, and on the other
// the handful of things the server says it was waiting for.
func (s *SlowQueries) WaitTotals() (total float64, waits []Wait) {
	s.flush()
	sums, seen := map[string]float64{}, map[string]Wait{}
	for _, shape := range s.shapes {
		total += shape.Duration.Sum
		for _, wait := range shape.Waits() {
			sums[wait.Name] += wait.Millis
			seen[wait.Name] = wait
		}
	}
	if total <= 0 {
		return 0, nil
	}
	for name, millis := range sums {
		entry := seen[name]
		entry.Millis, entry.Share = millis, millis/total
		waits = append(waits, entry)
	}
	sort.Slice(waits, func(i, j int) bool {
		if waits[i].Millis != waits[j].Millis {
			return waits[i].Millis > waits[j].Millis
		}
		return waits[i].Name < waits[j].Name
	})
	return total, waits
}

// Collscans counts the shapes and executions that read a collection end to end.
func (s *SlowQueries) Collscans() (shapes, executions int) {
	s.flush()
	for _, shape := range s.shapes {
		if shape.Plan == "COLLSCAN" {
			shapes++
			executions += shape.Count
		}
	}
	return shapes, executions
}

// operationOf reads the operation and, where the command names it, the collection it ran against.
// A command line reports its namespace as "db.$cmd"; the collection is the command's own value.
func operationOf(attributes slowAttributes) (operation, collection string) {
	if len(attributes.Command) > 0 {
		var command map[string]json.RawMessage
		if json.Unmarshal(attributes.Command, &command) == nil {
			// The catalogue commands are here as well as in the DDL reader: a createIndexes that
			// ran long enough to be logged is a slow query like any other, and leaving it out of
			// this list filed it as "unknown" against the "db.$cmd" namespace.
			for _, name := range []string{
				"find", "aggregate", "update", "delete", "insert", "getMore", "count", "distinct", "findAndModify",
				"createIndexes", "dropIndexes", "create", "drop", "dropDatabase", "renameCollection",
				"collMod", "shardCollection", "reshardCollection", "refineCollectionShardKey",
				"compact", "validate", "mapReduce", "listIndexes", "listCollections", "explain", "bulkWrite",
			} {
				raw, present := command[name]
				if !present {
					continue
				}
				var target string
				if json.Unmarshal(raw, &target) != nil {
					target = "" // aggregate against a database uses 1 rather than a collection name
				}
				return name, target
			}
		}
	}
	if attributes.Type != "" {
		return attributes.Type, ""
	}
	return "unknown", ""
}

// namespaceOf resolves "db.$cmd" to the collection the command actually touched, so one collection
// does not scatter across as many rows as it has query shapes.
func namespaceOf(namespace, collection string) string {
	if collection == "" || !strings.HasSuffix(namespace, ".$cmd") {
		return namespace
	}
	return strings.TrimSuffix(namespace, "$cmd") + collection
}

// planOf reduces a plan summary to the scan type that decides whether an index was used.
func planOf(summary string) string {
	upper := strings.ToUpper(strings.TrimSpace(summary))
	switch {
	case upper == "":
		return "-"
	case strings.Contains(upper, "COLLSCAN"):
		return "COLLSCAN"
	case strings.Contains(upper, "IXSCAN"):
		return "IXSCAN"
	case strings.Contains(upper, "IDHACK"):
		return "IDHACK"
	case strings.Contains(upper, "COUNT_SCAN"):
		return "COUNT"
	case strings.Contains(upper, "EOF"):
		return "EOF"
	}
	return upper
}

// failureOf reads whether the server said this operation succeeded, and what it called the failure.
//
// A slow line reports a duration whatever the outcome. On a shard that cannot read its routing
// table, every query runs until maxTimeMS and fails with StaleConfig; on a router with no primary
// to talk to, every hello fails with FailedToSatisfyReadPreference. Both show up as the slowest
// things in the bundle, and without this they are indistinguishable from queries that worked.
func failureOf(attributes slowAttributes) (string, bool) {
	if attributes.ErrName != "" {
		return attributes.ErrName, true
	}
	if attributes.ErrCode != 0 {
		return "error " + strconv.Itoa(attributes.ErrCode), true
	}
	// ok is absent from the lines that succeeded, so only a logged zero is a failure. A plain
	// float would read an absent field as zero and call every successful operation a failure.
	if attributes.OK != nil && *attributes.OK == 0 {
		return "failed", true
	}
	return "", false
}

// FailureRate is the share of this shape's executions the server rejected.
func (s Shape) FailureRate() float64 {
	if s.Count == 0 {
		return 0
	}
	return float64(s.Failed) / float64(s.Count)
}

// DominantError is the failure this shape mostly hit, and how many of its executions hit it.
func (s Shape) DominantError() (string, int) {
	name, count := "", 0
	for candidate, seen := range s.Errors {
		if seen > count || (seen == count && candidate < name) {
			name, count = candidate, seen
		}
	}
	return name, count
}

// Outcome is the one phrase a table column can carry: what went wrong, and how often.
func (s Shape) Outcome() string {
	if s.Failed == 0 {
		return "ok"
	}
	name, _ := s.DominantError()
	if name == "" {
		name = "failed"
	}
	if s.Failed == s.Count {
		return "all " + name
	}
	// Rounding up to 100% for a shape that had successes says the opposite of what happened: the
	// interesting fact about 998 failures out of 1,000 is the two that got through.
	percent := min(int(math.Round(s.FailureRate()*100)), 99)
	return strconv.Itoa(percent) + "% " + name
}

// Failures counts the executions and the time the server spent on operations that did not succeed.
func (s *SlowQueries) Failures() (executions int, millis float64, byName map[string]int) {
	s.flush()
	byName = map[string]int{}
	for _, shape := range s.shapes {
		executions += shape.Failed
		millis += shape.FailedMillis
		for name, count := range shape.Errors {
			byName[name] += count
		}
	}
	return executions, millis, byName
}

// loggedHash returns the server-provided identifier verbatim. No identifier is invented.
func loggedHash(attributes slowAttributes) string {
	if attributes.PlanCacheShape != "" {
		return attributes.PlanCacheShape
	}
	return attributes.QueryHash
}

// shapeID is an internal composite key, never a reported query shape identifier.
// Without a server hash, only a namespace/operation/plan group can be established.
func shapeID(attributes slowAttributes, namespace, operation, plan string) (string, string) {
	groupedBy := "namespace"
	if attributes.PlanCacheShape != "" {
		groupedBy = "planCacheShapeHash"
	} else if attributes.QueryHash != "" {
		groupedBy = "queryHash"
	}
	key, _ := json.Marshal([]string{namespace, operation, plan, groupedBy, loggedHash(attributes)})
	return string(key), groupedBy
}

// Label makes missing evidence explicit instead of displaying a synthetic hash.
func (s Shape) Label() string {
	if s.ID == "" {
		return "not logged"
	}
	return s.ID
}
