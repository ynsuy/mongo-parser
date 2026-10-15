package cli

import (
	"testing"
	"time"

	"github.com/ynsuy/mongo-parser/internal/store"
)

func TestQualityDistinguishesNotesFromUnreadableFiles(t *testing.T) {
	st, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, source := range []struct{ path, kind, note string }{
		{"duplicate", "ftdc", "an exact copy of metrics; read once"},
		{"export", "log_csv", "times are the export's timestamp column, read as UTC"},
		{"legacy", "log_legacy_text", "kept the first 16 MiB only"},
		{"broken", "other: unreadable gzip", ""},
	} {
		if _, err := st.DB.Exec(`INSERT INTO source(path, kind, note) VALUES(?, ?, ?)`, source.path, source.kind, source.note); err != nil {
			t.Fatal(err)
		}
	}
	first := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	if _, err := st.DB.Exec(`INSERT INTO stream(source, first_ts, last_ts, lines) VALUES(1, ?, ?, 2)`, first.UnixMilli(), first.Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	q, err := readQuality(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Unreadable) != 1 || q.Unreadable["broken"] != 1 {
		t.Fatalf("informational notes counted as unreadable: %+v", q)
	}
	if len(q.SourceNotes) != 3 || len(q.NotRead) != 1 || q.NotRead[0] != "legacy" {
		t.Fatalf("coverage notes lost: %+v", q)
	}
	if !q.FirstAt.Equal(first) || !q.LastAt.Equal(first.Add(time.Minute)) {
		t.Fatalf("log time bounds lost: %+v", q)
	}
}
