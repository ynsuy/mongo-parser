package store

import (
	"database/sql"
	"sort"
	"time"
)

// Run is one run of a node's process, as its FTDC recorded it.
type Run struct {
	Node       string    `json:"node"`
	Role       string    `json:"role,omitempty"`
	Set        string    `json:"set,omitempty"`
	Started    time.Time `json:"started"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
	Version    string    `json:"version,omitempty"`
	CacheBytes int64     `json:"cache_bytes,omitempty"`
	MemKB      int64     `json:"mem_kb,omitempty"`
	CPUs       int64     `json:"cpus,omitempty"`
}

// sameRun is how close two FTDC streams' start times for one node have to be to be the same run: a
// node's diagnostic.data is often in a bundle twice, collected at different times.
const sameRun = 30 * time.Second

// Runs returns every node's runs, oldest first, with the copies two FTDC streams of one node make
// of the same run joined.
func (s *Store) Runs() ([]Run, error) {
	rows, err := s.DB.Query(`SELECT CASE WHEN n.label != '' THEN n.label ELSE n.name END, n.role, n.replset, r.started_ts, r.first_ts, r.last_ts,
		r.version, r.cache_bytes, r.mem_kb, r.cpus FROM ftdc_run r JOIN node n ON n.id = r.node ORDER BY r.node, r.started_ts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started, first, last int64
		if err := rows.Scan(&r.Node, &r.Role, &r.Set, &started, &first, &last, &r.Version, &r.CacheBytes, &r.MemKB, &r.CPUs); err != nil {
			return nil, err
		}
		r.Started, r.First, r.Last = time.UnixMilli(started).UTC(), time.UnixMilli(first).UTC(), time.UnixMilli(last).UTC()
		if n := len(out); n > 0 && out[n-1].Node == r.Node && out[n-1].Started.Sub(r.Started).Abs() <= sameRun {
			prev := &out[n-1]
			if r.First.Before(prev.First) {
				prev.First = r.First
			}
			if r.Last.After(prev.Last) {
				prev.Last = r.Last
			}
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SystemEvent is one event of an operating system's log.
type SystemEvent struct {
	Node    string    `json:"node,omitempty"` // empty when the machine is not one node's alone
	Host    string    `json:"host,omitempty"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Process string    `json:"process,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Clock   string    `json:"clock,omitempty"` // how its time was read, when the line did not say its zone
	File    string    `json:"file"`
	Line    int       `json:"line"`
	Raw     string    `json:"raw"`
}

// SystemEvents returns the events in [from, to], oldest first; zero times leave a side open.
func (s *Store) SystemEvents(from, to time.Time) ([]SystemEvent, error) {
	lo, hi := int64(-1<<62), int64(1<<62)
	if !from.IsZero() {
		lo = from.UnixMilli()
	}
	if !to.IsZero() {
		hi = to.UnixMilli()
	}
	rows, err := s.DB.Query(`SELECT COALESCE(CASE WHEN n.label != '' THEN n.label ELSE n.name END, ''), e.host, e.ts, e.kind, e.process,
		e.detail, e.clock, src.path, e.line, e.raw FROM os_event e JOIN source src ON src.id = e.source LEFT JOIN node n ON n.id = e.node
		WHERE e.ts BETWEEN ? AND ? ORDER BY e.ts`, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SystemEvent
	for rows.Next() {
		var e SystemEvent
		var at int64
		var node sql.NullString
		if err := rows.Scan(&node, &e.Host, &at, &e.Kind, &e.Process, &e.Detail, &e.Clock, &e.File, &e.Line, &e.Raw); err != nil {
			return nil, err
		}
		e.Node, e.At = node.String, time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, rows.Err()
}
