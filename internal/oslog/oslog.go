// Package oslog reads the few lines of an operating system's logs that say why a database process
// went away or a machine restarted: the kernel's OOM killer, a full file table, a task hung on I/O,
// a disk error, a segfault, a boot, and what systemd said about the mongod unit.
//
// Everything else in those logs is left alone. They are not in every bundle, they come in several
// formats under any name, and most of what they hold is noise next to the server's own log — but
// when a mongod was killed, they are the only place that says so.
package oslog

import (
	"bufio"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Event is one such line.
type Event struct {
	Line    int
	Host    string // the machine the line names, as written; empty for dmesg
	Kind    string
	Process string // the process the line is about: the one killed, the unit
	Detail  string
	Raw     string

	// At is the line's time in UTC when it carried its zone. Otherwise Local is the time as written,
	// held as if it were UTC, and Year says whether the line wrote its year; the zone, and the year,
	// are settled later from what the rest of the bundle says.
	At     time.Time
	Local  time.Time
	Year   bool
	Drifts bool // dmesg -T: computed from the boot time, and late by however long the machine slept
}

// Kinds of event.
const (
	OOMKill      = "oom_kill"
	FileMax      = "file_max"
	HungTask     = "hung_task"
	Segfault     = "segfault"
	IOError      = "io_error"
	Boot         = "boot"
	ServiceStart = "service_start"
	ServiceStop  = "service_stop"
	ServiceExit  = "service_exit"
)

// Anchor is a line that wrote its time twice: once as the log's own local time, once inside the
// message with its zone. The difference between the two is the log's zone.
type Anchor struct {
	Local time.Time // as written, held as if UTC
	Year  bool
	UTC   time.Time
}

// File is what one log yielded.
type File struct {
	Events  []Event
	Anchors []Anchor
	Year    int // a year the file names in a header, for lines that leave it out
	Lines   int // lines in a format this package reads
}

const maxAnchors = 64

// Scan reads a whole log.
func Scan(r io.Reader) (*File, error) {
	f := &File{}
	reader := bufio.NewReaderSize(r, 1<<20)
	number := 0
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			number++
			f.Observe(strings.TrimRight(line, "\r\n"), number)
		}
		if err == io.EOF {
			return f, nil
		}
		if err != nil {
			return f, err
		}
	}
}

var (
	isoLine     = regexp.MustCompile(`^(\d{4}-\d\d-\d\d[T ]\d\d:\d\d:\d\d(?:[.,]\d+)?(?:Z|[+-]\d\d:?\d\d))\s+(\S+)\s+(.*)$`)
	classicLine = regexp.MustCompile(`^([A-Z][a-z]{2})\s+(\d{1,2})\s+(\d\d:\d\d:\d\d)(?:\.\d+)?\s+(\S+)\s+(.*)$`)
	dmesgTLine  = regexp.MustCompile(`^\[\s*[A-Z][a-z]{2} ([A-Z][a-z]{2})\s+(\d{1,2}) (\d\d:\d\d:\d\d) (\d{4})\]\s?(.*)$`)
	program     = regexp.MustCompile(`^([^\s:\[]+)(?:\[(\d+)\])?:\s?(.*)$`)
	logsBegin   = regexp.MustCompile(`^-- Logs begin at \w+ (\d{4})-`)
	embedded    = regexp.MustCompile(`(\d{4}-\d\d-\d\d[T ]\d\d:\d\d:\d\d(?:\.\d+)?)\s?(Z|[+-]\d\d:?\d\d)\b`)
)

// Observe reads one line. It is also how the lines of a mixed file are offered — a journal that
// holds the server's JSON beside systemd's own lines — so it accepts anything and keeps what it knows.
func (f *File) Observe(line string, number int) {
	if line == "" {
		return
	}
	if line[0] == '-' {
		if m := logsBegin.FindStringSubmatch(line); m != nil {
			f.Year, _ = strconv.Atoi(m[1])
		}
		return
	}
	e := Event{Line: number}
	var text string
	switch {
	case line[0] >= '0' && line[0] <= '9':
		m := isoLine.FindStringSubmatch(line)
		if m == nil {
			return
		}
		at, ok := parseZoned(m[1])
		if !ok {
			return
		}
		e.At, e.Host, text = at, m[2], m[3]
	case line[0] == '[':
		m := dmesgTLine.FindStringSubmatch(line)
		if m == nil {
			return
		}
		local, ok := parseLocal(m[1], m[2], m[3], m[4])
		if !ok {
			return
		}
		e.Local, e.Year, e.Drifts, text = local, true, true, "kernel: "+m[5]
	case line[0] >= 'A' && line[0] <= 'Z':
		m := classicLine.FindStringSubmatch(line)
		if m == nil {
			return
		}
		local, ok := parseLocal(m[1], m[2], m[3], "")
		if !ok {
			return
		}
		e.Local, e.Host, text = local, m[4], m[5]
	default:
		return
	}
	f.Lines++
	if !e.Local.IsZero() && len(f.Anchors) < maxAnchors && strings.Contains(text, ":") {
		f.anchor(e, text)
	}
	prog, message := "", text
	if m := program.FindStringSubmatch(text); m != nil {
		prog, message = m[1], m[3]
	}
	if !classify(&e, prog, message) {
		return
	}
	e.Raw = line
	f.Events = append(f.Events, e)
}

// anchor keeps a line that also wrote its time with a zone, when the two are the same moment.
func (f *File) anchor(e Event, text string) {
	m := embedded.FindStringSubmatch(text)
	if m == nil {
		return
	}
	utc, ok := parseZoned(strings.Replace(m[1], " ", "T", 1) + m[2])
	if !ok {
		return
	}
	f.Anchors = append(f.Anchors, Anchor{Local: e.Local, Year: e.Year, UTC: utc})
}

var (
	oomKill    = regexp.MustCompile(`(?:Out of memory|Memory cgroup out of memory): Kill(?:ed)? process (\d+) \(([^)]+)\)(?:.*?anon-rss:(\d+)kB)?`)
	fileMax    = regexp.MustCompile(`VFS: file-max limit (\d+) reached`)
	hungTask   = regexp.MustCompile(`task (\S+?):(\d+) blocked for more than (\d+) seconds`)
	segfault   = regexp.MustCompile(`^(\S+?)\[(\d+)\]: segfault at`)
	ioError    = regexp.MustCompile(`I/O error|EXT4-fs error|XFS \([^)]+\): (?:metadata I/O error|Corruption)`)
	ioDevice   = regexp.MustCompile(`dev(?:ice)?:? \(?([\w./-]+?)\)?[,: ]|XFS \(([^)]+)\)`)
	mongoUnit  = regexp.MustCompile(`^(mongo[ds][\w@.-]*?)\.service: (.*)$`)
	unitStart  = regexp.MustCompile(`^(Started|Stopped) (?:(mongo[ds][\w@.-]*?)\.service\b|(?:MongoDB|mongod|mongos)\b|Percona Server for MongoDB|High-performance, schema-free document-oriented database)`)
	exitStatus = regexp.MustCompile(`^Main process exited, code=(\w+), status=(\S+)`)
)

// classify decides whether a line is an event, and fills it in.
func classify(e *Event, prog, message string) bool {
	switch {
	case prog == "kernel":
		switch {
		case strings.Contains(message, "out of memory") || strings.Contains(message, "Out of memory"):
			m := oomKill.FindStringSubmatch(message)
			if m == nil {
				return false
			}
			e.Kind, e.Process, e.Detail = OOMKill, m[2], "pid "+m[1]
			if kb, err := strconv.ParseInt(m[3], 10, 64); err == nil && kb > 0 {
				e.Detail += ", " + strconv.FormatFloat(float64(kb)/(1<<20), 'f', 1, 64) + " GiB resident"
			}
			return true
		case strings.Contains(message, "file-max"):
			m := fileMax.FindStringSubmatch(message)
			if m == nil {
				return false
			}
			e.Kind, e.Detail = FileMax, "limit "+m[1]
			return true
		case strings.Contains(message, "blocked for more than"):
			m := hungTask.FindStringSubmatch(message)
			if m == nil {
				return false
			}
			e.Kind, e.Process, e.Detail = HungTask, m[1], "blocked "+m[3]+" s"
			return true
		case strings.Contains(message, "segfault at"):
			// Another program's crash says nothing about the database.
			m := segfault.FindStringSubmatch(message)
			if m == nil || !strings.HasPrefix(m[1], "mongo") {
				return false
			}
			e.Kind, e.Process, e.Detail = Segfault, m[1], "pid "+m[2]
			return true
		case strings.HasPrefix(message, "Linux version "):
			e.Kind, e.Detail = Boot, strings.Fields(message)[2]
			return true
		case ioError.MatchString(message):
			e.Kind = IOError
			if m := ioDevice.FindStringSubmatch(message); m != nil {
				e.Detail = m[1] + m[2]
			}
			return true
		}
	case prog == "systemd":
		if m := mongoUnit.FindStringSubmatch(message); m != nil {
			if exit := exitStatus.FindStringSubmatch(m[2]); exit != nil {
				e.Kind, e.Process, e.Detail = ServiceExit, m[1], "code="+exit[1]+", status="+exit[2]
				return true
			}
			return false
		}
		if m := unitStart.FindStringSubmatch(message); m != nil {
			e.Kind, e.Process = ServiceStart, m[2]
			if m[1] == "Stopped" {
				e.Kind = ServiceStop
			}
			if e.Process == "" {
				e.Process = "mongod"
			}
			return true
		}
	}
	return false
}

var months = map[string]time.Month{"Jan": 1, "Feb": 2, "Mar": 3, "Apr": 4, "May": 5, "Jun": 6, "Jul": 7, "Aug": 8, "Sep": 9, "Oct": 10, "Nov": 11, "Dec": 12}

// parseLocal reads a time with no zone, as if UTC. Without a year it is put in year 0, for the
// caller to move once the year is known.
func parseLocal(month, day, clock, year string) (time.Time, bool) {
	m, ok := months[month]
	if !ok {
		return time.Time{}, false
	}
	d, err := strconv.Atoi(day)
	if err != nil {
		return time.Time{}, false
	}
	c, err := time.Parse("15:04:05", clock)
	if err != nil {
		return time.Time{}, false
	}
	y := 0
	if year != "" {
		if y, err = strconv.Atoi(year); err != nil {
			return time.Time{}, false
		}
	}
	return time.Date(y, m, d, c.Hour(), c.Minute(), c.Second(), 0, time.UTC), true
}

func parseZoned(text string) (time.Time, bool) {
	text = strings.Replace(strings.Replace(text, " ", "T", 1), ",", ".", 1)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999-0700", "2006-01-02T15:04:05-0700"} {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// InYear puts a time read without a year into the given one.
func InYear(local time.Time, year int) time.Time {
	return time.Date(year, local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), time.UTC)
}

// Zone reads a log's distance from UTC off its anchors: how far each line's own time is from the
// one written inside it, rounded to the quarter hour zones are made of. ok is false when the
// anchors do not mostly agree. year is the year the anchors were written in.
func (f *File) Zone() (offset time.Duration, year int, votes int, ok bool) {
	const step = 15 * time.Minute
	count := map[time.Duration]int{}
	years := map[int]int{}
	for _, a := range f.Anchors {
		local := a.Local
		if !a.Year {
			local = InYear(local, a.UTC.Year())
			// A line written on the 31st of December in a zone ahead of UTC, about a moment on the
			// 1st of January in UTC, is a year behind.
			if diff := local.Sub(a.UTC); diff > 180*24*time.Hour {
				local = local.AddDate(-1, 0, 0)
			} else if diff < -180*24*time.Hour {
				local = local.AddDate(1, 0, 0)
			}
		}
		diff := local.Sub(a.UTC)
		rounded := diff.Round(step)
		if residual := (diff - rounded).Abs(); residual > 2*time.Minute || rounded.Abs() > 14*time.Hour {
			continue
		}
		count[rounded]++
		years[local.Year()]++
	}
	total := 0
	for zone, n := range count {
		total += n
		if n > votes {
			offset, votes = zone, n
		}
	}
	for y, n := range years {
		if n > years[year] {
			year = y
		}
	}
	return offset, year, votes, total > 0 && votes*10 >= total*6
}
