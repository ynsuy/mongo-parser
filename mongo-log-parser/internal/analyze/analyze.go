// Package analyze turns stored log lines into the aggregates a report is made of: what was
// running, what was slow, what the set did, what changed in the catalogue, what the server
// complained about.
//
// Every aggregate here is computable in one pass, and Run makes that one pass: the store is read
// once and each line is offered to every sink. Nothing merges partial results, because nothing is
// computed twice.
package analyze

import (
	"encoding/json"
	"strings"
	"time"

	"mongo-log-parser/internal/store"
)

// Record is one log line as the aggregates see it: the stored line, plus the two things they ask
// of every line and the store keeps elsewhere — when it was written, and which node wrote it.
type Record struct {
	*store.Record
	At     time.Time
	Source string // the node's label, or the stream's when no node could be named
}

// Attributes decodes the attribute document. Most lines never need it, which is why the store
// keeps it as text.
func (r Record) Attributes() map[string]any {
	attr := r.Attr()
	if len(attr) == 0 {
		return nil
	}
	var decoded map[string]any
	if json.Unmarshal(attr, &decoded) != nil {
		return nil
	}
	return decoded
}

// DecodeAttributes reads the attributes into a typed value, which is far cheaper than a map when
// only a few fields are wanted.
func (r Record) DecodeAttributes(into any) bool {
	attr := r.Attr()
	return len(attr) > 0 && json.Unmarshal(attr, into) == nil
}

// Omitted is how many entries the server said it dropped from this line, added up wherever it said
// so. MongoDB caps an attribute document at a few kilobytes and stamps what it cut alongside the
// line, nesting the note the same way it nests the document it cut. Anything counted from a
// truncated line is a floor, and only this says so.
func (r Record) Omitted() int {
	if !r.Truncated {
		return 0
	}
	start := strings.IndexByte(r.Raw, '{')
	if start < 0 {
		return 0
	}
	var line struct {
		Truncated json.RawMessage `json:"truncated"`
	}
	if json.Unmarshal([]byte(r.Raw[start:]), &line) != nil || len(line.Truncated) == 0 {
		return 0
	}
	var note any
	if json.Unmarshal(line.Truncated, &note) != nil {
		return 0
	}
	return countOmitted(note)
}

func countOmitted(value any) int {
	total := 0
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if key == "omitted" {
				if count, ok := nested.(float64); ok {
					total += int(count)
				}
				continue
			}
			total += countOmitted(nested)
		}
	case []any:
		for _, nested := range typed {
			total += countOmitted(nested)
		}
	}
	return total
}

// Sink is one aggregate being filled, a line at a time.
type Sink interface{ Add(Record) }

// Run offers every line the query admits to every sink, oldest first.
func Run(st *store.Store, q store.Query, sinks ...Sink) error {
	labels, err := Labels(st)
	if err != nil {
		return err
	}
	return st.Scan(q, func(r *store.Record) bool {
		line := Record{Record: r, At: time.UnixMilli(r.TS).UTC(), Source: labels[r.Stream]}
		for _, sink := range sinks {
			sink.Add(line)
		}
		return true
	})
}

// Labels names the node behind each stream, so a line can say who wrote it without a join. A
// stream whose node could not be told apart answers with its own name.
func Labels(st *store.Store) (map[int64]string, error) {
	rows, err := st.DB.Query(`
		SELECT stream.id, coalesce(nullif(node.label, ''), nullif(node.name, ''), stream.instance, 'stream ' || stream.id)
		FROM stream LEFT JOIN node ON node.id = stream.node`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var id int64
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, err
		}
		labels[id] = label
	}
	return labels, rows.Err()
}
