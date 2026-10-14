package ingest

import (
	"sort"
	"strings"
	"time"

	"mongo-log-parser/internal/oslog"
	"mongo-log-parser/internal/source"
)

// The operating system's side of a node's story: the events its logs hold, placed in time and tied
// to the node. Syslog writes local time with no zone and no year, so both are settled here, from
// the strongest evidence there is: the log's own lines that also wrote a zoned time, then the same
// machine's other logs, then the zone mongod wrote its own log in. Where none of them says, the
// time is read as UTC and the event says so.

type systemLog struct {
	source int64
	path   string
	dir    string
	file   *oslog.File
}

// keepSystem keeps a log's events for placing once the nodes are known. Must be called with mu held.
func (in *ingester) keepSystem(sourceID int64, e *source.Entry, f *oslog.File) {
	if f != nil && len(f.Events) > 0 {
		in.system = append(in.system, systemLog{source: sourceID, path: e.Path, dir: e.Dir, file: f})
	}
}

type placed struct {
	oslog.Event
	source int64
	at     time.Time
	clock  string
	node   int64
	host   string
}

func (in *ingester) writeSystemEvents(nodes []*nodeRow) error {
	if len(in.system) == 0 {
		return nil
	}
	type zone struct {
		offset time.Duration
		year   int
	}
	own := map[int]zone{}
	byHost, byDir := map[string]time.Duration{}, map[string]time.Duration{}
	for i, log := range in.system {
		offset, year, _, ok := log.file.Zone()
		if !ok {
			continue
		}
		own[i] = zone{offset, year}
		byDir[log.dir] = offset
		for _, e := range log.file.Events {
			if e.Host != "" {
				host, _ := shortHost(e.Host)
				byHost[host] = offset
			}
		}
	}
	serverZone, serverSays := mongodZone(in.streams)
	reference := latest(nodes)

	hosts := map[string]map[int64]bool{}
	for _, n := range nodes {
		for _, name := range append([]string{n.Name}, n.Aliases...) {
			host, _ := shortHost(name)
			if host == "" {
				continue
			}
			if hosts[host] == nil {
				hosts[host] = map[int64]bool{}
			}
			hosts[host][n.ID] = true
		}
	}
	nodeOf := func(host, path string) (int64, string) {
		short, _ := shortHost(host)
		if ids := hosts[short]; short != "" && len(ids) == 1 {
			for id := range ids {
				return id, short
			}
		}
		if short != "" {
			return 0, short
		}
		where := strings.ToLower(path)
		found := map[int64]string{}
		for name, ids := range hosts {
			if len(ids) == 1 && len(name) >= 3 && namesHost(where, name) {
				for id := range ids {
					found[id] = name
				}
			}
		}
		if len(found) == 1 {
			for id, name := range found {
				return id, name
			}
		}
		if len(nodes) == 1 {
			short, _ := shortHost(nodes[0].Name)
			return nodes[0].ID, short
		}
		return 0, ""
	}

	var events []placed
	for i, log := range in.system {
		for _, e := range log.file.Events {
			p := placed{Event: e, source: log.source, at: e.At}
			if p.at.IsZero() {
				offset, year, clock := time.Duration(0), 0, ""
				short, _ := shortHost(e.Host)
				if z, ok := own[i]; ok {
					offset, year = z.offset, z.year
				} else if o, ok := byHost[short]; ok && short != "" {
					offset, clock = o, "zone taken from the machine's other logs"
				} else if o, ok := byDir[log.dir]; ok {
					offset, clock = o, "zone taken from the machine's other logs"
				} else if serverSays {
					offset, clock = serverZone, "zone taken from the times mongod wrote its own log in"
				} else {
					clock = "no zone known: read as UTC"
				}
				local := e.Local
				if !e.Year {
					if year == 0 {
						year = log.file.Year
					}
					if year == 0 {
						year = reference.Year()
						if oslog.InYear(local, year).After(reference.AddDate(0, 0, 2)) {
							year--
						}
					}
					local = oslog.InYear(local, year)
				}
				p.at = local.Add(-offset)
				p.clock = clock
			}
			if e.Drifts {
				p.clock = joinNote(p.clock, "dmesg time, which runs late by however long the machine slept")
			}
			p.node, p.host = nodeOf(e.Host, log.path)
			events = append(events, p)
		}
	}
	events = dropRepeats(events)

	tx, err := in.st.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range events {
		var node any
		if e.node != 0 {
			node = e.node
		}
		if _, err := tx.Exec(`INSERT INTO os_event(source, line, node, host, ts, kind, process, detail, raw, clock) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			e.source, e.Line, node, e.host, e.at.UnixMilli(), e.Kind, e.Process, e.Detail, e.Raw, e.clock); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// dropRepeats keeps one of the copies of an event that several logs recorded — the kernel's lines
// go to dmesg, the journal and syslog alike — preferring the one whose time is exact.
func dropRepeats(events []placed) []placed {
	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	var out []placed
	for _, e := range events {
		window := 2 * time.Second
		if e.Drifts {
			window = 10 * time.Minute
		}
		repeat := -1
		for k := len(out) - 1; k >= 0; k-- {
			o := out[k]
			limit := window
			if o.Drifts {
				limit = 10 * time.Minute
			}
			if e.at.Sub(o.at) > limit {
				break
			}
			if o.Kind == e.Kind && o.Process == e.Process && o.Detail == e.Detail && (o.host == e.host || o.host == "" || e.host == "") {
				repeat = k
				break
			}
		}
		switch {
		case repeat < 0:
			out = append(out, e)
		case out[repeat].Drifts && !e.Drifts:
			out[repeat] = e
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// mongodZone is the zone the server logs were written in, when they wrote local time and agree.
func mongodZone(streams []streamRow) (time.Duration, bool) {
	zones := map[string]bool{}
	for _, s := range streams {
		if z := s.Stats.Zone; z != "" && z != "Z" {
			zones[z] = true
		}
	}
	if len(zones) != 1 {
		return 0, false
	}
	for z := range zones {
		t, err := time.Parse("-07:00", z)
		if err != nil {
			return 0, false
		}
		_, seconds := t.Zone()
		return time.Duration(seconds) * time.Second, true
	}
	return 0, false
}

// latest is the last moment anything in the bundle recorded, which a date written without its year
// is assumed not to be far after.
func latest(nodes []*nodeRow) time.Time {
	var last int64
	for _, n := range nodes {
		for _, it := range n.members {
			if it.stream != nil {
				last = max(last, it.stream.Stats.Last)
			} else if it.ftdc != nil {
				last = max(last, it.ftdc.Last)
			}
		}
	}
	if last == 0 {
		return time.Now().UTC()
	}
	return time.UnixMilli(last).UTC()
}
