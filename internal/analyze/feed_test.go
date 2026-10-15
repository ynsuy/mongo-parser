package analyze

import (
	"strings"
	"testing"
	"time"

	"github.com/ynsuy/mongo-parser/internal/ingest"
	"github.com/ynsuy/mongo-parser/internal/store"
)

// feed parses log text the way ingest does and offers each line to the sinks, as Run does over a
// store. source is the node the lines are attributed to: a test that needs two nodes calls feed
// twice with different names, which is what writing two files used to do.
func feed(t *testing.T, source, text string, sinks ...Sink) {
	t.Helper()
	if _, err := ingest.ReadJSONLog(strings.NewReader(text), func(r store.Record) {
		line := r
		rec := Record{Record: &line, At: time.UnixMilli(line.TS).UTC(), Source: source}
		for _, sink := range sinks {
			sink.Add(rec)
		}
	}); err != nil {
		t.Fatalf("reading log text: %v", err)
	}
}

// rec builds one record from its parts, for a test that is about an attribute document rather than
// about a log line.
func rec(message, attr string) Record {
	line := &store.Record{Msg: message}
	line.SetAttr(attr)
	return Record{Record: line}
}

// at is rec with a time, for the aggregates that bin by it.
func at(when time.Time, message, attr string) Record {
	r := rec(message, attr)
	r.TS = when.UnixMilli()
	r.At = when.UTC()
	return r
}
