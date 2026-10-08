package analyze

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// A node doing an initial sync writes, every few seconds, the one line in a MongoDB log that says
// how much data there is.
//
// Nothing else does. serverStatus carries no collection sizes, FTDC carries the oplog's and the
// system collections' and no others, and dbStats is a command nobody ran into a log. So "how big is
// this deployment" is a question a bundle usually cannot answer at all — unless a member was
// rebuilding from a peer while the logs were being collected, and then the answer is complete down
// to the document count and index count of every collection it had reached.
//
// It is also the line that says why a rebuild is not finishing, which is the reason anybody is
// reading it: every attempt is listed with the error that ended it.

// ClonedCollection is one collection as the initial sync found it on the sync source.
type ClonedCollection struct {
	Namespace string `json:"ns"`
	// Bytes and Documents are the sync source's own count of what is there, which is what makes
	// this an inventory of the deployment rather than a progress report.
	Bytes     float64 `json:"bytes"`
	Documents float64 `json:"documents"`
	Indexes   int     `json:"indexes"`
	// Copied is how far the clone got. A collection whose copy equals its size was finished.
	Copied float64 `json:"bytes_copied"`
	// Millis is how long the clone took, which is the figure that says which collection a stalled
	// rebuild is stuck on.
	Millis float64 `json:"elapsed_ms"`
	Done   bool    `json:"cloned"`
}

// SyncAttempt is one try at an initial sync and what ended it.
type SyncAttempt struct {
	Millis     float64 `json:"duration_ms"`
	Status     string  `json:"status"`
	SyncSource string  `json:"sync_source,omitempty"`
	// Unreachable is how long the source could not be reached during the attempt, which separates
	// an attempt that failed on this node from one that failed on the network.
	Unreachable float64 `json:"unreachable_ms"`
}

// Failed reports whether this attempt ended in anything other than success.
func (a SyncAttempt) Failed() bool {
	return a.Status != "" && !strings.EqualFold(a.Status, "OK") && !strings.HasPrefix(a.Status, "{ ok: 1")
}

// Rebuild is the latest statistics line one node wrote about the initial sync it was doing.
type Rebuild struct {
	Source string    `json:"source"`
	At     time.Time `json:"-"`
	Status string    `json:"status,omitempty"`
	Method string    `json:"method,omitempty"`

	Started       time.Time `json:"-"`
	ElapsedMillis float64   `json:"elapsed_ms"`
	RemainingMs   float64   `json:"estimated_remaining_ms"`

	FailedAttempts int           `json:"failed_attempts"`
	MaxAttempts    int           `json:"max_attempts"`
	Attempts       []SyncAttempt `json:"attempts,omitempty"`

	// TotalBytes is the deployment's own figure for how much data the sync source holds.
	TotalBytes  float64 `json:"total_bytes"`
	BytesCopied float64 `json:"bytes_copied"`

	DatabasesToClone int                `json:"databases_to_clone"`
	DatabasesCloned  int                `json:"databases_cloned"`
	Collections      []ClonedCollection `json:"collections,omitempty"`

	// Truncated is the server saying it cut this line short. MongoDB caps an attribute document at
	// a few kilobytes and stamps what it dropped; a statistics line for a deployment of any size is
	// over that cap, so the collection list below is the ones that fitted and not the ones there
	// are. Reporting a total from the rows rather than from the server's own figure would be
	// wrong by however much was omitted, and this is what says so.
	Truncated bool `json:"truncated"`
	// Omitted is how many entries the server said it dropped, where it said.
	Omitted int `json:"omitted,omitempty"`
}

// Progress is how far the copy had got, as a fraction.
func (s Rebuild) Progress() float64 {
	if s.TotalBytes <= 0 {
		return 0
	}
	return s.BytesCopied / s.TotalBytes
}

// LastFailure is the error that ended the most recent failed attempt.
func (s Rebuild) LastFailure() string {
	for i := len(s.Attempts) - 1; i >= 0; i-- {
		if s.Attempts[i].Failed() {
			return s.Attempts[i].Status
		}
	}
	return ""
}

// Reason shortens a failure to the part that names it. MongoDB chains its causes — "<outer error>:
// <what it was doing> :: caused by :: ... :: caused by :: <inner error>: <what stopped it>" — and
// the two ends are what a reader needs: what it was doing, and what actually stopped it.
func Reason(status string) string {
	if status == "" {
		return ""
	}
	parts := strings.Split(status, " :: caused by :: ")
	first := strings.TrimSpace(parts[0])
	if len(parts) == 1 {
		return first
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	if last == first {
		return first
	}
	return first + " — " + last
}

// Rebuilds collects the statistics lines of a scan.
type Rebuilds struct {
	// byNode keeps the latest line per node: the server rewrites the whole picture every few
	// seconds, so the last one is the most complete and the earlier ones are prefixes of it.
	byNode map[string]*Rebuild
}

func NewRebuilds() *Rebuilds { return &Rebuilds{byNode: map[string]*Rebuild{}} }

// initialSyncAttributes is the shape of the line. Only what is read is named; the rest of a
// statistics document is progress bookkeeping that repeats what these say.
type initialSyncAttributes struct {
	Status     string `json:"status"`
	Statistics struct {
		Method                       string   `json:"method"`
		FailedInitialSyncAttempts    int      `json:"failedInitialSyncAttempts"`
		MaxFailedInitialSyncAttempts int      `json:"maxFailedInitialSyncAttempts"`
		InitialSyncStart             jsonDate `json:"initialSyncStart"`
		TotalElapsedMillis           float64  `json:"totalInitialSyncElapsedMillis"`
		RemainingMillis              float64  `json:"remainingInitialSyncEstimatedMillis"`
		ApproxTotalDataSize          float64  `json:"approxTotalDataSize"`
		ApproxTotalBytesCopied       float64  `json:"approxTotalBytesCopied"`
		Attempts                     []struct {
			DurationMillis float64 `json:"durationMillis"`
			Status         string  `json:"status"`
			SyncSource     string  `json:"syncSource"`
			Unreachable    float64 `json:"totalTimeUnreachableMillis"`
		} `json:"initialSyncAttempts"`
		Databases json.RawMessage `json:"databases"`
	} `json:"statistics"`
}

// jsonDate reads MongoDB's {"$date": ...}, which is how every timestamp inside an attribute is
// written and is not the plain string the outer "t" field uses.
type jsonDate struct{ time.Time }

func (d *jsonDate) UnmarshalJSON(data []byte) error {
	var wrapper struct {
		Date json.RawMessage `json:"$date"`
	}
	if json.Unmarshal(data, &wrapper) != nil || len(wrapper.Date) == 0 {
		return nil
	}
	var text string
	if json.Unmarshal(wrapper.Date, &text) == nil {
		if at, err := time.Parse(time.RFC3339, text); err == nil {
			d.Time = at
		}
		return nil
	}
	var millis int64
	if json.Unmarshal(wrapper.Date, &millis) == nil {
		d.Time = time.UnixMilli(millis).UTC()
	}
	return nil
}

// collectionEntry is one collection inside a database's document.
type collectionEntry struct {
	DocumentsToCopy   float64  `json:"documentsToCopy"`
	DocumentsCopied   float64  `json:"documentsCopied"`
	Indexes           int      `json:"indexes"`
	BytesToCopy       float64  `json:"bytesToCopy"`
	ApproxBytesCopied float64  `json:"approxBytesCopied"`
	ElapsedMillis     float64  `json:"elapsedMillis"`
	End               jsonDate `json:"end"`
}

// Add folds one record in; anything that is not a statistics line is ignored.
func (s *Rebuilds) Add(record Record) {
	if record.Msg != "Initial sync status and statistics" {
		return
	}
	var attributes initialSyncAttributes
	if !record.DecodeAttributes(&attributes) {
		return
	}
	sync := &Rebuild{
		Source: record.Source, At: record.At, Status: attributes.Status,
		Method:         attributes.Statistics.Method,
		Started:        attributes.Statistics.InitialSyncStart.Time,
		ElapsedMillis:  attributes.Statistics.TotalElapsedMillis,
		RemainingMs:    attributes.Statistics.RemainingMillis,
		FailedAttempts: attributes.Statistics.FailedInitialSyncAttempts,
		MaxAttempts:    attributes.Statistics.MaxFailedInitialSyncAttempts,
		TotalBytes:     attributes.Statistics.ApproxTotalDataSize,
		BytesCopied:    attributes.Statistics.ApproxTotalBytesCopied,
		Truncated:      record.Truncated,
		Omitted:        record.Omitted(),
	}
	for _, attempt := range attributes.Statistics.Attempts {
		sync.Attempts = append(sync.Attempts, SyncAttempt{
			Millis: attempt.DurationMillis, Status: attempt.Status,
			SyncSource: attempt.SyncSource, Unreachable: attempt.Unreachable,
		})
	}
	sync.DatabasesToClone, sync.DatabasesCloned, sync.Collections = readDatabases(attributes.Statistics.Databases)

	// The server rewrites the whole picture every few seconds. Later is more complete.
	if existing, seen := s.byNode[record.Source]; !seen || record.At.After(existing.At) {
		s.byNode[record.Source] = sync
	}
}

// readDatabases walks the databases document, which is not a list of anything: a database's entry
// holds its own counters and then one key per collection, named for the collection, mixed in
// beside them. So the collections are whatever keys carry a dot.
func readDatabases(raw json.RawMessage) (toClone, cloned int, collections []ClonedCollection) {
	if len(raw) == 0 {
		return 0, 0, nil
	}
	var databases map[string]json.RawMessage
	if json.Unmarshal(raw, &databases) != nil {
		return 0, 0, nil
	}
	for name, value := range databases {
		switch name {
		case "databasesToClone":
			json.Unmarshal(value, &toClone)
			continue
		case "databasesCloned":
			json.Unmarshal(value, &cloned)
			continue
		}
		var database map[string]json.RawMessage
		if json.Unmarshal(value, &database) != nil {
			continue
		}
		for key, entry := range database {
			if !strings.Contains(key, ".") {
				continue // a counter of the database's own, not a collection
			}
			var stats collectionEntry
			if json.Unmarshal(entry, &stats) != nil {
				continue
			}
			collections = append(collections, ClonedCollection{
				Namespace: key, Bytes: stats.BytesToCopy, Documents: stats.DocumentsToCopy,
				Indexes: stats.Indexes, Copied: stats.ApproxBytesCopied, Millis: stats.ElapsedMillis,
				// A collection with no end stamp is the one the clone was on when the line was
				// written, which is the one a stalled rebuild is stuck on.
				Done: !stats.End.IsZero(),
			})
		}
	}
	sort.Slice(collections, func(i, j int) bool {
		if collections[i].Bytes != collections[j].Bytes {
			return collections[i].Bytes > collections[j].Bytes
		}
		return collections[i].Namespace < collections[j].Namespace
	})
	return toClone, cloned, collections
}

// All is what every node said about the rebuild it was doing, newest first.
func (s *Rebuilds) All() []Rebuild {
	out := make([]Rebuild, 0, len(s.byNode))
	for _, sync := range s.byNode {
		out = append(out, *sync)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// Largest is the deployment's data as the most complete statistics line described it.
//
// One node's line is enough: every member of a set holds the same data, and a sync source's figure
// for its own size is the deployment's. Where several nodes were rebuilding, the one that got
// furthest saw the most.
func (s *Rebuilds) Largest() (Rebuild, bool) {
	best, found := Rebuild{}, false
	for _, sync := range s.byNode {
		if !found || sync.TotalBytes > best.TotalBytes ||
			(sync.TotalBytes == best.TotalBytes && len(sync.Collections) > len(best.Collections)) {
			best, found = *sync, true
		}
	}
	return best, found
}
