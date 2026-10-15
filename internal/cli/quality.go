package cli

import (
	"strings"
	"time"

	"github.com/ynsuy/mongo-parser/internal/store"
)

// quality is what the store could and could not read, so a report never implies it saw everything
// when it did not. The bundle was read once, at ingest; this reads back what that pass recorded.
type quality struct {
	Files       int               `json:"files"`
	Lines       int               `json:"lines_read"`
	Parsed      int               `json:"records_parsed"`
	Invalid     int               `json:"unparsable_lines"`
	Truncated   int               `json:"truncated_by_server"`
	NotRead     []string          `json:"not_read,omitempty"` // named like a log, held none this build can read
	Unreadable  map[string]int    `json:"unreadable_files,omitempty"`
	SourceNotes map[string]string `json:"source_notes,omitempty"`

	// FirstAt and LastAt bound what the logs actually cover, which is not the window that was
	// asked for: a question about an hour nobody logged has no answer, and only this says so.
	FirstAt time.Time `json:"first_at,omitzero"`
	LastAt  time.Time `json:"last_at,omitzero"`
}

// readQuality gathers it from the store.
func readQuality(st *store.Store) (quality, error) {
	q := quality{Unreadable: map[string]int{}, SourceNotes: map[string]string{}}
	var first, last int64
	err := st.DB.QueryRow(`
		SELECT count(*), coalesce(sum(lines), 0), coalesce(sum(bad_lines), 0), coalesce(sum(truncated_lines), 0),
		       coalesce(min(first_ts), 0), coalesce(max(last_ts), 0)
		FROM stream`).Scan(&q.Files, &q.Parsed, &q.Invalid, &q.Truncated, &first, &last)
	if err != nil {
		return q, err
	}
	q.Lines = q.Parsed + q.Invalid
	if first > 0 {
		q.FirstAt, q.LastAt = time.UnixMilli(first).UTC(), time.UnixMilli(last).UTC()
	}

	// A file whose name made a claim and whose contents this build cannot read is not noise to be
	// dropped quietly: a 4.2 text log beside the structured ones is the difference between "these
	// logs are empty" and "these logs are a format nothing here parses yet".
	rows, err := st.DB.Query(`SELECT path, kind, note FROM source WHERE kind IN ('log_legacy_text', 'other') OR kind LIKE '%unreadable%' OR note != ''`)
	if err != nil {
		return q, err
	}
	defer rows.Close()
	for rows.Next() {
		var path, kind, note string
		if err := rows.Scan(&path, &kind, &note); err != nil {
			return q, err
		}
		if note != "" {
			q.SourceNotes[path] = note
		}
		if strings.Contains(kind, "unreadable") || strings.Contains(strings.ToLower(note), "unreadable") {
			q.Unreadable[path]++
		}
		if kind == "log_legacy_text" {
			q.NotRead = append(q.NotRead, path)
		}
	}
	return q, rows.Err()
}
