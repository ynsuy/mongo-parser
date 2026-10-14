package analyze

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"mongo-log-parser/internal/store"
)

// A catalogue change is the thing a log can explain that no chart can. An index build takes a
// collection lock in its final phase, a drop invalidates every plan that referenced it, and a
// shardCollection rewrites where the data lives. Each of them turns "everything got slow at 02:00"
// into a sentence, and none of them shows up in a metric as anything but the symptom.

// IndexBuild is one build, followed from the line that started it to the line that ended it.
type IndexBuild struct {
	UUID      string    `json:"build_uuid,omitempty"`
	Namespace string    `json:"ns"`
	Indexes   string    `json:"indexes,omitempty"`
	Source    string    `json:"source"`
	Started   time.Time `json:"started_at,omitzero"`
	Finished  time.Time `json:"finished_at,omitzero"`
	Outcome   string    `json:"outcome"`
	Phases    int       `json:"phases"`
}

// Duration is how long the build ran, or zero while it is still only a start.
func (b IndexBuild) Duration() time.Duration {
	if b.Finished.IsZero() || !b.Finished.After(b.Started) {
		return 0
	}
	return b.Finished.Sub(b.Started)
}

// Change is a family of catalogue changes: the same kind of operation on the same namespace.
type Change struct {
	Kind      string         `json:"kind"`
	Namespace string         `json:"ns,omitempty"`
	Count     int            `json:"count"`
	Failures  int            `json:"failures"`
	First     time.Time      `json:"first_at,omitzero"`
	Last      time.Time      `json:"last_at,omitzero"`
	Sources   map[string]int `json:"sources"`
	Example   string         `json:"example,omitempty"`
	MaxMillis float64        `json:"max_millis,omitempty"`
}

// maxBuilds bounds the builds one scan keeps. A rebuild of every index of a large deployment is
// hundreds, not thousands, and the Changes table still counts what falls off the end.
const maxBuilds = 500

// DDL folds catalogue changes out of a log stream.
type DDL struct {
	builds  map[string]*IndexBuild // the ones still running, by build id or by namespace
	closed  []*IndexBuild          // the ones that ended, in the order they ended
	changes map[string]*Change
	decided map[string]string // message text -> the kind it means, or "" for none
}

func NewDDL() *DDL {
	return &DDL{builds: map[string]*IndexBuild{}, changes: map[string]*Change{}, decided: map[string]string{}}
}

// ddlCommands are the commands that change the catalogue. They are read off the command document of
// a slow query line, which is how a DDL that took long enough to be logged is caught whatever the
// server called the message: the command's own name does not change between versions.
// ddlCommandNames are ddlCommands' names, for a scan of a command's keys.
var ddlCommandNames = func() []string {
	names := make([]string, 0, len(ddlCommands))
	for name := range ddlCommands {
		names = append(names, name)
	}
	return names
}()

var ddlCommands = map[string]string{
	"createIndexes":                  "create index",
	"dropIndexes":                    "drop index",
	"create":                         "create collection",
	"drop":                           "drop collection",
	"dropDatabase":                   "drop database",
	"renameCollection":               "rename collection",
	"collMod":                        "collMod",
	"shardCollection":                "shard collection",
	"reshardCollection":              "reshard collection",
	"refineCollectionShardKey":       "refine shard key",
	"compact":                        "compact",
	"validate":                       "validate",
	"convertToCapped":                "convert to capped",
	"cloneCollectionAsCapped":        "clone as capped",
	"setFeatureCompatibilityVersion": "set FCV",
}

// ddlMessages are the dedicated lines a server writes for a catalogue change, for the changes that
// finish too quickly to be logged as slow queries.
var ddlMessages = map[string]string{
	"createCollection":                    "create collection",
	"Dropping collection":                 "drop collection",
	"dropCollection":                      "drop collection",
	"Finishing collection drop":           "drop collection",
	"dropDatabase":                        "drop database",
	"renameCollection":                    "rename collection",
	"renameCollectionForCommand":          "rename collection",
	"Sharding collection":                 "shard collection",
	"Created initial chunk(s)":            "shard collection",
	"Refining collection shard key":       "refine shard key",
	"Registering index build":             "create index",
	"Creating index":                      "create index",
	"Setting featureCompatibilityVersion": "set FCV",
}

type ddlAttributes struct {
	Namespace  string          `json:"namespace"`
	NS         string          `json:"ns"`
	Collection string          `json:"collection"`
	Database   string          `json:"db"`
	Indexes    json.RawMessage `json:"indexes"`
	Spec       json.RawMessage `json:"spec"`
	Properties json.RawMessage `json:"properties"`
	BuildUUID  json.RawMessage `json:"buildUUID"`
	UUID       json.RawMessage `json:"uuid"`
	Error      json.RawMessage `json:"error"`
	Reason     string          `json:"reason"`
	Duration   float64         `json:"durationMillis"`
	Command    json.RawMessage `json:"command"`
	Type       string          `json:"type"`
}

// Add folds one record into the catalogue picture.
func (d *DDL) Add(record Record) {
	if strings.HasPrefix(record.Msg, "Index build: ") {
		d.addIndexBuild(record)
		return
	}
	if record.Msg == "Slow query" {
		d.addSlowDDL(record)
		return
	}
	kind, known := d.decided[record.Msg]
	if !known {
		kind = ddlKindOf(record.Msg)
		d.decided[record.Msg] = kind
	}
	if kind == "" {
		return
	}
	var attributes ddlAttributes
	record.DecodeAttributes(&attributes)
	namespace := namespaceIn(attributes)
	if namespace == "" {
		namespace = commandTarget(record.Msg)
	}
	d.change(kind, namespace, record, 0, len(attributes.Error) > 0 || attributes.Reason != "")
}

// ddlKindOf names the catalogue change a message reports, or "" for a message that reports none.
// The "CMD: " prefix is the server's own convention for a command it is about to run, so the
// fallback catches the ones not listed by name.
func ddlKindOf(message string) string {
	if kind, listed := ddlMessages[message]; listed {
		return kind
	}
	for prefix, kind := range ddlMessages {
		if strings.HasPrefix(message, prefix) {
			return kind
		}
	}
	if rest, found := strings.CutPrefix(message, "CMD: "); found {
		name := strings.Fields(rest)
		if len(name) == 0 {
			return ""
		}
		if kind, listed := ddlCommands[name[0]]; listed {
			return kind
		}
		return strings.ToLower(name[0])
	}
	return ""
}

// commandTarget reads the namespace out of a "CMD: drop app.orders" line. These carry no attributes
// at all on some versions, and a drop with no collection name beside it is a row that tells a
// reader something happened and refuses to say to what.
func commandTarget(message string) string {
	rest, found := strings.CutPrefix(message, "CMD: ")
	if !found {
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return ""
	}
	// The second word is the namespace only when it looks like one; "CMD: fsync lock" is not a
	// collection called lock.
	if !strings.Contains(fields[1], ".") {
		return ""
	}
	return fields[1]
}

// addSlowDDL reads a catalogue change out of a slow query line, which is where a long one lands.
func (d *DDL) addSlowDDL(record Record) {
	// Nearly every slow query is a read or a write; only a command naming a DDL command at its top
	// level is decoded. The scan matches keys ignoring case, so it never passes over one the decode
	// below would have taken.
	if command, found, ok := store.Field(string(record.Attr()), "command"); ok {
		if !found {
			return
		}
		if names, ok := store.Fields(command, ddlCommandNames...); ok {
			named := false
			for _, value := range names {
				named = named || value != ""
			}
			if !named {
				return
			}
		}
	}
	var attributes ddlAttributes
	if !record.DecodeAttributes(&attributes) || len(attributes.Command) == 0 {
		return
	}
	var command map[string]json.RawMessage
	if json.Unmarshal(attributes.Command, &command) != nil {
		return
	}
	for name, kind := range ddlCommands {
		raw, present := command[name]
		if !present {
			continue
		}
		namespace := namespaceIn(attributes)
		var target string
		if json.Unmarshal(raw, &target) == nil && target != "" && strings.HasSuffix(namespace, ".$cmd") {
			namespace = strings.TrimSuffix(namespace, "$cmd") + target
		}
		d.change(kind, namespace, record, attributes.Duration, false)
		return
	}
}

func (d *DDL) addIndexBuild(record Record) {
	var attributes ddlAttributes
	record.DecodeAttributes(&attributes)
	phase := strings.ToLower(strings.TrimPrefix(record.Msg, "Index build: "))
	key := uuidOf(attributes.BuildUUID)
	if key == "" {
		// Without a build id the phases of two concurrent builds cannot be told apart; the
		// namespace is the next best key and is wrong only when the same collection is built twice
		// at once, which the phase count then makes visible.
		key = record.Source + "\x00" + namespaceIn(attributes)
	}
	build := d.builds[key]
	if build == nil {
		if len(d.builds)+len(d.closed) >= maxBuilds {
			// Still counted as a change, so a bundle full of builds does not silently lose them.
			d.change("create index", namespaceIn(attributes), record, 0, strings.Contains(phase, "fail"))
			return
		}
		build = &IndexBuild{
			UUID: uuidOf(attributes.BuildUUID), Namespace: namespaceIn(attributes), Source: record.Source,
			Started: record.At, Outcome: "running",
		}
		d.builds[key] = build
	}
	build.Phases++
	if record.At.Before(build.Started) {
		build.Started = record.At
	}
	if build.Namespace == "" {
		build.Namespace = namespaceIn(attributes)
	}
	if build.Indexes == "" {
		build.Indexes = indexNames(attributes)
	}
	switch {
	case strings.Contains(phase, "fail"):
		build.Outcome, build.Finished = "failed", record.At
	case strings.Contains(phase, "abort"):
		build.Outcome, build.Finished = "aborted", record.At
	case strings.Contains(phase, "done building"), strings.Contains(phase, "completed"),
		strings.Contains(phase, "commit"), strings.Contains(phase, "index build finished"):
		build.Outcome, build.Finished = "completed", record.At
	}
	// A build that has ended is closed to further lines. Without it, a server that rebuilds the
	// same index at every startup folded every one of those rebuilds into a single row, and
	// reported a short build as one that ran from the first startup to the last. The key is the
	// namespace whenever the server logged no build id, and a namespace is reused by design.
	if build.Outcome != "running" {
		d.closed = append(d.closed, build)
		delete(d.builds, key)
	}
}

// change counts one catalogue change against its kind and namespace.
func (d *DDL) change(kind, namespace string, record Record, millis float64, failed bool) {
	key := kind + "\x00" + namespace
	entry := d.changes[key]
	if entry == nil {
		entry = &Change{Kind: kind, Namespace: namespace, First: record.At, Sources: map[string]int{}, Example: record.Msg}
		d.changes[key] = entry
	}
	entry.Count++
	entry.Sources[record.Source]++
	if failed {
		entry.Failures++
	}
	if millis > entry.MaxMillis {
		entry.MaxMillis = millis
	}
	if record.At.Before(entry.First) {
		entry.First = record.At
	}
	if record.At.After(entry.Last) {
		entry.Last = record.At
	}
}

// Builds returns the index builds, the longest first: a build that ran for hours is the one that
// explains the window around it.
func (d *DDL) Builds() []IndexBuild {
	out := make([]IndexBuild, 0, len(d.builds)+len(d.closed))
	for _, build := range d.closed {
		out = append(out, *build)
	}
	for _, build := range d.builds {
		out = append(out, *build)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Duration() != out[j].Duration() {
			return out[i].Duration() > out[j].Duration()
		}
		return out[i].Started.Before(out[j].Started)
	})
	return out
}

// Changes returns the catalogue changes, the most recent family first, because a change is usually
// read against something that happened after it.
func (d *DDL) Changes() []Change {
	out := make([]Change, 0, len(d.changes))
	for _, change := range d.changes {
		out = append(out, *change)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Failures != out[j].Failures {
			return out[i].Failures > out[j].Failures
		}
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].Kind+out[i].Namespace < out[j].Kind+out[j].Namespace
	})
	return out
}

// Counts is how many changes of each kind there were, for a line that has room for one sentence.
func (d *DDL) Counts() map[string]int {
	counts := map[string]int{}
	for _, change := range d.changes {
		counts[change.Kind] += change.Count
	}
	if n := len(d.builds) + len(d.closed); n > 0 {
		counts["index build"] += n
	}
	return counts
}

// FailedBuilds counts the builds that did not finish cleanly. A failed build is a closed one, so
// the running map holds none of them; it is read anyway, because that is a property of how builds
// are closed and not something this should depend on.
func (d *DDL) FailedBuilds() int {
	failed := 0
	for _, build := range d.closed {
		if build.Outcome == "failed" || build.Outcome == "aborted" {
			failed++
		}
	}
	for _, build := range d.builds {
		if build.Outcome == "failed" || build.Outcome == "aborted" {
			failed++
		}
	}
	return failed
}

// namespaceIn reads whichever field this line used to name what it touched.
func namespaceIn(attributes ddlAttributes) string {
	if name := firstNonEmpty(attributes.Namespace, attributes.NS, attributes.Collection); name != "" {
		return name
	}
	return attributes.Database
}

// uuidOf unwraps the {"uuid":{"$uuid":"..."}} shape a build id is written in.
func uuidOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var wrapped struct {
		UUID struct {
			Value string `json:"$uuid"`
		} `json:"uuid"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.UUID.Value != "" {
		return wrapped.UUID.Value
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}
	return ""
}

// indexNames renders whatever this line said about which indexes were being built.
func indexNames(attributes ddlAttributes) string {
	for _, raw := range []json.RawMessage{attributes.Properties, attributes.Spec, attributes.Indexes} {
		if len(raw) == 0 {
			continue
		}
		var names []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &names) == nil && len(names) > 0 {
			list := make([]string, 0, len(names))
			for _, entry := range names {
				if entry.Name != "" {
					list = append(list, entry.Name)
				}
			}
			if len(list) > 0 {
				return strings.Join(list, ", ")
			}
		}
		var single struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &single) == nil && single.Name != "" {
			return single.Name
		}
	}
	return ""
}
