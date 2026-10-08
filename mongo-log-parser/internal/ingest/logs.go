package ingest

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"mongo-log-parser/internal/store"
)

// Identity is what a stream's own lines say about the process that wrote them. It is the only
// trustworthy way to tie a file to a node: names of files and directories are whatever the person
// collecting them chose.
type Identity struct {
	Self        string   `json:"self,omitempty"` // "Found self in config": its name in the replica set config
	Host        string   `json:"host,omitempty"` // "MongoDB starting": the machine's hostname
	Port        int      `json:"port,omitempty"`
	Pod         string   `json:"pod,omitempty"`         // the instance column of a log export
	Process     string   `json:"process,omitempty"`     // mongod or mongos
	ClusterRole string   `json:"clusterRole,omitempty"` // shardsvr / configsvr
	Versions    []string `json:"versions,omitempty"`    // in the order they were seen
	ReplSet     string   `json:"replSet,omitempty"`
	DBPath      string   `json:"dbPath,omitempty"`
	Starts      int      `json:"starts"`
}

func isIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return false
		}
	}
	return true
}

func (id *Identity) version(v string) {
	if v == "" {
		return
	}
	if n := len(id.Versions); n == 0 || id.Versions[n-1] != v {
		id.Versions = append(id.Versions, v)
	}
}

// observe reads identity out of the handful of startup lines that carry it. The server restates
// these on every log rotation, so a rotated file usually identifies itself too.
func (id *Identity) observe(msg, ctx string, attr []byte) {
	switch msg {
	case "MongoDB starting":
		var a struct {
			Host   string `json:"host"`
			Port   int    `json:"port"`
			DBPath string `json:"dbPath"`
		}
		if json.Unmarshal(attr, &a) == nil {
			if a.Host != "" {
				id.Host = a.Host
			}
			if a.Port != 0 {
				id.Port = a.Port
			}
			if a.DBPath != "" {
				id.DBPath = a.DBPath
			}
		}
		if ctx == "mongosMain" {
			id.Process = "mongos"
		} else if id.Process == "" {
			id.Process = "mongod"
		}
		if ctx == "initandlisten" || ctx == "mongosMain" {
			id.Starts++
		}
	case "Build Info":
		if ctx == "mongosMain" {
			id.Process = "mongos"
		}
		var a struct {
			BuildInfo struct {
				Version string `json:"version"`
			} `json:"buildInfo"`
		}
		if json.Unmarshal(attr, &a) == nil {
			id.version(a.BuildInfo.Version)
		}
	case "Options set by command line":
		var a struct {
			Options struct {
				Net struct {
					Port   int    `json:"port"`
					BindIP string `json:"bindIp"`
				} `json:"net"`
				Replication struct {
					ReplSetName string `json:"replSetName"`
					ReplSet     string `json:"replSet"`
				} `json:"replication"`
				Sharding struct {
					ClusterRole string `json:"clusterRole"`
				} `json:"sharding"`
				Storage struct {
					DBPath string `json:"dbPath"`
				} `json:"storage"`
			} `json:"options"`
		}
		if ctx == "mongosMain" {
			id.Process = "mongos"
		}
		if json.Unmarshal(attr, &a) == nil {
			o := a.Options
			if o.Net.Port != 0 {
				id.Port = o.Net.Port
			}
			// A mongos states no name of its own anywhere else; the address it binds to is one.
			if id.Host == "" {
				for _, addr := range strings.Split(o.Net.BindIP, ",") {
					addr = strings.TrimSpace(addr)
					if strings.Contains(addr, ".") && !strings.HasPrefix(addr, "127.") && addr != "0.0.0.0" && !isIPv4(addr) {
						id.Host = addr
						break
					}
				}
			}
			if o.Replication.ReplSetName != "" {
				id.ReplSet = o.Replication.ReplSetName
			} else if o.Replication.ReplSet != "" {
				id.ReplSet = strings.SplitN(o.Replication.ReplSet, "/", 2)[0]
			}
			if o.Sharding.ClusterRole != "" {
				id.ClusterRole = o.Sharding.ClusterRole
			}
			if o.Storage.DBPath != "" {
				id.DBPath = o.Storage.DBPath
			}
		}
	case "Found self in config":
		var a struct {
			HostAndPort string `json:"hostAndPort"`
		}
		if json.Unmarshal(attr, &a) == nil && a.HostAndPort != "" {
			id.Self = a.HostAndPort
		}
	}
}

// Stats is what reading one stream found, beyond its rows.
type Stats struct {
	Lines      int
	BadLines   int
	BadExample string
	First      int64
	Last       int64
	Unparsed   int // exported lines whose attributes could not be converted and stay text
	Identity   Identity
}

func (s *Stats) bad(line string) {
	s.BadLines++
	if s.BadExample == "" {
		if len(line) > 300 {
			line = line[:300] + "…"
		}
		s.BadExample = line
	}
}

func (s *Stats) seen(ts int64) {
	s.Lines++
	if s.First == 0 || ts < s.First {
		s.First = ts
	}
	if ts > s.Last {
		s.Last = ts
	}
}

const maxLine = 64 << 20

// readLines calls fn with each line and its 1-based number, without the trailing newline. Lines
// longer than maxLine are cut, and say so in the store rather than stopping the read.
func readLines(r io.Reader, fn func(line []byte, number int) error) error {
	reader := bufio.NewReaderSize(r, 1<<20)
	number := 0
	var long []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(long) < maxLine {
				long = append(long, chunk...)
			}
			continue
		}
		line := chunk
		if long != nil {
			long = append(long, chunk...)
			line = long
		}
		if len(line) > 0 {
			number++
			line = bytes.TrimRight(line, "\r\n")
			if ferr := fn(line, number); ferr != nil {
				return ferr
			}
		}
		long = nil
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type jsonLine struct {
	T struct {
		Date json.RawMessage `json:"$date"`
	} `json:"t"`
	S         string          `json:"s"`
	C         string          `json:"c"`
	ID        int             `json:"id"`
	Ctx       string          `json:"ctx"`
	Msg       string          `json:"msg"`
	Attr      json.RawMessage `json:"attr"`
	Truncated json.RawMessage `json:"truncated"`
}

// parseDate reads the t.$date of a structured log line: an ISO string, or {"$numberLong": ...}
// when the server was told to log timestamps as epoch milliseconds.
func parseDate(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, false
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05.000-07:00"} {
			if t, err := time.Parse(layout, text); err == nil {
				return t.UnixMilli(), true
			}
		}
		return 0, false
	}
	var long struct {
		N json.Number `json:"$numberLong"`
	}
	if json.Unmarshal(raw, &long) == nil {
		if n, err := long.N.Int64(); err == nil {
			return n, true
		}
	}
	return 0, false
}

// ReadJSONLog reads MongoDB's structured log. Lines that are not log lines (a banner, a fragment
// at a cut) are counted, not dropped silently.
func ReadJSONLog(r io.Reader, emit func(store.Record)) (Stats, error) {
	var stats Stats
	err := readLines(r, func(line []byte, number int) error {
		if len(bytes.TrimSpace(line)) == 0 {
			return nil
		}
		brace := bytes.IndexByte(line, '{')
		if brace < 0 || brace > 512 {
			stats.bad(string(line))
			return nil
		}
		var parsed jsonLine
		if err := json.Unmarshal(line[brace:], &parsed); err != nil {
			stats.bad(string(line))
			return nil
		}
		ts, ok := parseDate(parsed.T.Date)
		if !ok {
			stats.bad(string(line))
			return nil
		}
		rec := store.Record{Line: number, TS: ts, Sev: parsed.S, Comp: parsed.C, LogID: parsed.ID,
			Ctx: parsed.Ctx, Msg: parsed.Msg, Raw: string(line), Truncated: len(parsed.Truncated) > 0}
		if brace > 0 && len(parsed.Attr) > 0 {
			// Something prefixed the line, so the attributes are kept apart from it.
			rec.SetAttr(string(parsed.Attr))
		}
		stats.seen(ts)
		stats.Identity.observe(parsed.Msg, parsed.Ctx, parsed.Attr)
		emit(rec)
		return nil
	})
	return stats, err
}

// csvColumns maps what a log export calls its columns to what they mean. Exports differ in naming;
// the first name present wins.
var csvColumns = map[string][]string{
	"time":     {"source_timestamp", "timestamp", "@timestamp", "time"},
	"severity": {"severity", "s"},
	"comp":     {"component", "c"},
	"ctx":      {"context_", "context", "ctx"},
	"message":  {"message", "msg"},
	"instance": {"instance-id", "pod", "host", "hostname", "kubernetes.pod_name"},
	"replset":  {"replicaset", "replica_set"},
	"id":       {"id", "log_id"},
	// Some exports do not print "msg, attr={...}" into the message at all: they split the line's
	// attributes into columns of their own, as JSON. Without these the line keeps its text and
	// loses every number in it — a slow query with no namespace, no plan and a duration of zero.
	"attrs":    {"query_meta", "attr", "attributes"},
	"command":  {"query", "command"},
	"duration": {"duration", "durationmillis"},
}

var csvTimeLayouts = []string{"2006-01-02 15:04:05.000", "2006-01-02 15:04:05", time.RFC3339Nano, "2006-01-02T15:04:05.000"}

// CSVStream is one instance's share of a log export.
type CSVStream struct {
	Instance string
	Stats    Stats
}

// ReadCSVLog reads a log export: one MongoDB line per row, the attributes printed as
// "msg, attr={...}" in Python's notation. Every instance in the export is its own stream; streamFor
// hands out the stream id for an instance the first time it appears.
func ReadCSVLog(r io.Reader, emit func(instance string, rec store.Record)) (map[string]*CSVStream, string, error) {
	streams := map[string]*CSVStream{}
	var header map[string]int
	var delimiter rune
	var pending []byte
	pendingLine := 0
	note := ""
	err := readLines(r, func(line []byte, number int) error {
		if header == nil {
			delimiter = ';'
			if bytes.Count(line, []byte(",")) > bytes.Count(line, []byte(";")) {
				delimiter = ','
			}
			fields, err := splitCSV(line, delimiter)
			if err != nil {
				return err
			}
			header = map[string]int{}
			for i, f := range fields {
				header[strings.ToLower(strings.TrimSpace(f))] = i
			}
			for _, name := range csvColumns["time"] {
				if _, ok := header[name]; ok {
					note = "times are the export's " + name + " column, read as UTC"
					break
				}
			}
			return nil
		}
		// A quoted field may hold a newline; a record is complete when its quotes balance.
		if pending == nil {
			pendingLine = number
			pending = append([]byte(nil), line...)
		} else {
			pending = append(append(pending, '\n'), line...)
		}
		if bytes.Count(pending, []byte(`"`))%2 == 1 && len(pending) < maxLine {
			return nil
		}
		record := pending
		pending = nil
		fields, err := splitCSV(record, delimiter)
		if err != nil {
			streamStats(streams, "").bad(string(record))
			return nil
		}
		get := func(key string) string {
			for _, name := range csvColumns[key] {
				if i, ok := header[name]; ok && i < len(fields) && fields[i] != "" {
					return fields[i]
				}
			}
			return ""
		}
		instance := get("instance")
		stats := streamStats(streams, instance)
		ts, ok := parseCSVTime(get("time"))
		if !ok {
			stats.bad(string(record))
			return nil
		}
		msg, attrText := splitMessage(get("message"))
		msg, plan := splitPlanSummary(msg)
		attr, truncated := "null", false
		if attrText != "" {
			converted, rest, err := pyPrefix(attrText)
			rest = strings.TrimSpace(rest)
			if err != nil || (rest != "" && !strings.HasPrefix(rest, ", truncated=")) {
				blob, _ := json.Marshal(map[string]string{"_unparsed": attrText})
				converted = string(blob)
				stats.Unparsed++
			}
			attr, truncated = converted, strings.HasPrefix(rest, ", truncated=")
		} else if columns, ok := columnAttr(get("attrs"), get("command"), get("duration"), plan); ok {
			attr = columns
		} else if get("attrs") != "" {
			blob, _ := json.Marshal(map[string]string{"_unparsed": get("attrs")})
			attr = string(blob)
			stats.Unparsed++
		}
		rec := store.Record{Line: pendingLine, TS: ts, Sev: get("severity"), Comp: get("comp"),
			Ctx: get("ctx"), Msg: msg, Raw: string(record), Truncated: truncated}
		rec.SetAttr(attr)
		if id := get("id"); id != "" {
			if n, err := json.Number(id).Int64(); err == nil {
				rec.LogID = int(n)
			}
		}
		stats.seen(ts)
		stats.Identity.Pod = instance
		if rs := get("replset"); rs != "" && stats.Identity.ReplSet == "" {
			stats.Identity.ReplSet = rs
		}
		stats.Identity.observe(msg, rec.Ctx, []byte(attr))
		emit(instance, rec)
		return nil
	})
	return streams, note, err
}

func streamStats(streams map[string]*CSVStream, instance string) *Stats {
	s, ok := streams[instance]
	if !ok {
		s = &CSVStream{Instance: instance}
		streams[instance] = s
	}
	return &s.Stats
}

func splitCSV(line []byte, delimiter rune) ([]string, error) {
	reader := csv.NewReader(bytes.NewReader(line))
	reader.Comma = delimiter
	reader.LazyQuotes = true
	reader.FieldsPerRecord = -1
	return reader.Read()
}

func parseCSVTime(text string) (int64, bool) {
	for _, layout := range csvTimeLayouts {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// columnAttr rebuilds a line's attributes from the columns an export split them into.
//
// MongoDB writes one attr document per line. An export that keeps it whole in a column of its own
// usually lifts the command out into a second column, which is the same document with `command`
// taken out of it — so putting it back is a reconstruction and not a guess. The duration column is
// the same number the attributes already carry and is only used when they do not carry it.
//
// Anything that is not a JSON object is refused rather than wrapped, because a line whose
// attributes were not understood has to be counted as such.
func columnAttr(meta, command, duration, plan string) (string, bool) {
	meta, command, duration = strings.TrimSpace(meta), strings.TrimSpace(command), strings.TrimSpace(duration)
	plan = strings.TrimSpace(plan)
	fields := map[string]json.RawMessage{}
	if meta != "" {
		if !strings.HasPrefix(meta, "{") || json.Unmarshal([]byte(meta), &fields) != nil {
			return "", false
		}
	}
	if strings.HasPrefix(command, "{") && json.Valid([]byte(command)) {
		if _, carried := fields["command"]; !carried {
			fields["command"] = json.RawMessage(command)
		}
	}
	// The attribute column usually repeats the plan the message carried; when it does not, the
	// message is the only place it was written down.
	if _, carried := fields["planSummary"]; !carried && plan != "" {
		if blob, err := json.Marshal(plan); err == nil {
			fields["planSummary"] = json.RawMessage(blob)
		}
	}
	if _, carried := fields["durationMillis"]; !carried && duration != "" {
		if n, err := json.Number(duration).Int64(); err == nil {
			fields["durationMillis"] = json.RawMessage(strconv.FormatInt(n, 10))
		}
	}
	if len(fields) == 0 {
		return "", false
	}
	blob, err := json.Marshal(fields)
	if err != nil {
		return "", false
	}
	return string(blob), true
}

// splitPlanSummary peels a ", planSummary=..." tail off a message.
//
// Some log exports do not keep the message MongoDB wrote. They render the line the way a human
// reads it and fold the plan into the text, so a slow query arrives as
// "Slow query, planSummary=EXPRESS_IXSCAN { _id: 1 },EXPRESS_UPDATE" rather than "Slow query".
// Everything downstream matches the message exactly — the slow-query fold, the DDL reader, the
// log groups — so left alone these lines are not slow queries at all, and they are precisely the
// ones that carry a plan. Unrecognised, they would be dropped silently.
//
// The plan itself contains commas and braces, so the tail is everything after the first marker,
// not up to the next separator.
func splitPlanSummary(message string) (string, string) {
	const marker = ", planSummary="
	at := strings.Index(message, marker)
	if at < 0 {
		return message, ""
	}
	return message[:at], message[at+len(marker):]
}

// splitMessage separates "msg text, attr={...}" into the message and the attribute text.
func splitMessage(message string) (string, string) {
	at := strings.Index(message, ", attr={")
	if at < 0 || !strings.HasSuffix(message, "}") {
		return message, ""
	}
	return message[:at], message[at+len(", attr="):]
}
