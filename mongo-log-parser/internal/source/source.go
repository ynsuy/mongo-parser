// Package source finds what a bundle holds. A bundle is whatever a customer uploaded: a directory
// of logs, an archive of archives, a mongod.log split into pieces for mailing, a collector's
// output beside it. Every file is classified by what is in it, never by its name alone: a name can
// claim a kind the file is not, and a file of a known kind can carry any name at all.
package source

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Kind string

const (
	LogJSON   Kind = "log_json"        // MongoDB 4.4+ structured log
	LogCSV    Kind = "log_csv"         // a log export (Kubernetes/rsyslog) with one MongoDB line per row
	LogLegacy Kind = "log_legacy_text" // MongoDB 4.2 and older text log, named in coverage and deliberately not read
	FTDC      Kind = "ftdc"            // diagnostic.data
	Shell     Kind = "shell_output"    // rs.conf(), rs.status(), serverStatus ... as printed by a shell
	OSLog     Kind = "os_log"          // /var/log/messages, syslog, dmesg
	Collector Kind = "collector"       // pt-summary, pt-stalk, mdiag, PBM and the like
	Config    Kind = "config"          // mongod.conf / mongos.conf
	Foreign   Kind = "foreign"         // another database's files (MySQL, MariaDB, PostgreSQL)
	Other     Kind = "other"
)

// Entry is one file of the bundle, wherever it sits.
type Entry struct {
	Path      string // where to find it: relative to the root, with "!" between archive levels
	Container string // the archive chain it came out of, empty for a file on disk
	Dir       string // its directory, archive-qualified: FTDC files in one Dir are one stream
	Name      string
	Size      int64
	Kind      Kind
	Sub       string // for Shell and OSLog: which output it is (rs_conf, messages, dmesg ...)
	OnDisk    string // the real path when the file sits on disk uncompressed and whole
	Parts     int    // how many split pieces were joined to make it
	Note      string // what is wrong with it, when something is
}

// Visit receives each entry with a reader over its decompressed contents. The reader is only good
// until Visit returns: entries inside an archive are read off one stream in order.
type Visit func(entry *Entry, content io.Reader) error

const sniffBytes = 64 << 10

var splitPart = regexp.MustCompile(`^(.*)_part_([a-z]{2,})$`)

// TopLevel lists the units Walk can be given independently: each file on disk, with split pieces
// grouped. They are independent of each other, so a caller may walk them in parallel.
func TopLevel(root string) ([][]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return [][]string{{root}}, nil
	}
	var files []string
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == "__MACOSX" || (strings.HasPrefix(name, ".") && p != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || !d.Type().IsRegular() {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	// A unit is one file, or every piece of one split file in suffix order; units keep the order
	// their first file had.
	var units [][]string
	group := map[string]int{}
	for _, file := range files {
		if m := splitPart.FindStringSubmatch(file); m != nil {
			if at, seen := group[m[1]]; seen {
				units[at] = append(units[at], file)
				continue
			}
			group[m[1]] = len(units)
		}
		units = append(units, []string{file})
	}
	return units, nil
}

// Walk classifies one unit from TopLevel and hands every entry in it to visit, descending into
// archives. tmp is where an archive that cannot be streamed (a zip inside another archive) is
// written while it is read.
func Walk(root string, unit []string, tmp string, visit Visit) error {
	rel, _ := filepath.Rel(root, unit[0])
	if rel == "." || strings.HasPrefix(rel, "..") {
		rel = filepath.Base(unit[0])
	}
	var size int64
	readers := make([]io.Reader, 0, len(unit))
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	for _, part := range unit {
		f, err := os.Open(part)
		if err != nil {
			return err
		}
		closers = append(closers, f)
		if info, err := f.Stat(); err == nil {
			size += info.Size()
		}
		readers = append(readers, f)
	}
	entry := &Entry{Path: rel, Dir: filepath.Dir(rel), Name: filepath.Base(unit[0]), Size: size, Parts: len(unit)}
	if splitPart.MatchString(unit[0]) {
		entry.Path = splitPart.ReplaceAllString(rel, "$1")
		entry.Name = filepath.Base(entry.Path)
		if missing := missingPieces(unit); len(missing) > 0 {
			entry.Note = "split pieces missing from the upload: " + strings.Join(missing, ", ")
			if !strings.HasSuffix(unit[0], "_part_aa") {
				entry.Note += "; without the first piece nothing in this file can be read"
			} else {
				entry.Note += "; only what precedes the first gap can be read"
			}
		}
	}
	onDisk := ""
	if len(unit) == 1 {
		onDisk = unit[0]
	}
	return dispatch(entry, io.MultiReader(readers...), onDisk, tmp, visit, 0)
}

// dispatch decompresses, sniffs and either descends into an archive or hands the file to visit.
func dispatch(entry *Entry, r io.Reader, onDisk, tmp string, visit Visit, depth int) error {
	if depth > 6 {
		entry.Kind, entry.Sub = Other, "archive nested too deep"
		return visit(entry, bytes.NewReader(nil))
	}
	buffered := bufio.NewReaderSize(r, sniffBytes)
	head, _ := buffered.Peek(sniffBytes)
	var content io.Reader = buffered
	if bytes.HasPrefix(head, []byte{0x1f, 0x8b}) {
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			entry.Kind, entry.Sub = Other, "unreadable gzip: "+err.Error()
			return visit(entry, bytes.NewReader(nil))
		}
		defer gz.Close()
		gz.Multistream(true)
		inner := bufio.NewReaderSize(gz, sniffBytes)
		head, _ = inner.Peek(sniffBytes)
		content = inner
		onDisk = ""
	}

	switch {
	case isTar(head):
		return walkTar(entry, content, tmp, visit, depth)
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return walkZip(entry, content, onDisk, tmp, visit, depth)
	}
	entry.Kind, entry.Sub = classify(entry.Name, head)
	if entry.Kind == FTDC || entry.Kind == Other {
		entry.OnDisk = onDisk
	}
	return visit(entry, content)
}

// missingPieces names the suffixes absent from a split sequence, which runs aa, ab, ac ... up to
// the last piece present.
func missingPieces(unit []string) []string {
	have := map[string]bool{}
	last := ""
	for _, part := range unit {
		suffix := splitPart.FindStringSubmatch(part)[2]
		have[suffix] = true
		if suffix > last {
			last = suffix
		}
	}
	if len(last) != 2 {
		return nil
	}
	var missing []string
	for a := 'a'; a <= rune(last[0]); a++ {
		for b := 'a'; b <= 'z'; b++ {
			suffix := string([]rune{a, b})
			if suffix > last {
				break
			}
			if !have[suffix] {
				missing = append(missing, suffix)
			}
		}
	}
	return missing
}

func isTar(head []byte) bool {
	return len(head) >= 262 && bytes.Equal(head[257:262], []byte("ustar"))
}

func child(parent *Entry, name string, size int64) *Entry {
	container := parent.Path
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	return &Entry{
		Path:      container + "!" + clean,
		Container: container,
		Dir:       container + "!" + path.Dir(clean),
		Name:      path.Base(clean),
		Size:      size,
	}
}

func skipName(name string) bool {
	base := path.Base(name)
	return strings.HasPrefix(base, "._") || strings.Contains(name, "__MACOSX/") || base == ".DS_Store"
}

func walkTar(parent *Entry, r io.Reader, tmp string, visit Visit, depth int) error {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// A truncated upload is common. Everything read so far stands; say where it stopped.
			bad := child(parent, "<truncated archive>", 0)
			bad.Kind, bad.Sub = Other, "archive ends early: "+err.Error()
			return visit(bad, bytes.NewReader(nil))
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if skipName(header.Name) {
			continue
		}
		if err := dispatch(child(parent, header.Name, header.Size), tr, "", tmp, visit, depth+1); err != nil {
			return err
		}
	}
}

func walkZip(parent *Entry, r io.Reader, onDisk, tmp string, visit Visit, depth int) error {
	name := onDisk
	if name == "" {
		f, err := os.CreateTemp(tmp, "zip-*")
		if err != nil {
			return err
		}
		name = f.Name()
		defer os.Remove(name)
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	zr, err := zip.OpenReader(name)
	if err != nil {
		parent.Kind, parent.Sub = Other, "unreadable zip: "+err.Error()
		return visit(parent, bytes.NewReader(nil))
	}
	defer zr.Close()
	for _, file := range zr.File {
		if file.FileInfo().IsDir() || skipName(file.Name) {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			continue
		}
		err = dispatch(child(parent, file.Name, int64(file.UncompressedSize64)), rc, "", tmp, visit, depth+1)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

var (
	legacyLogLine = regexp.MustCompile(`(?m)^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}[+-]\d{4} [IWEFD]\d? +[A-Z_-]+ +\[`)
	syslogLine    = regexp.MustCompile(`(?m)^(?:[A-Z][a-z]{2} [ \d]\d \d{2}:\d{2}:\d{2}|\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:?\d\d)) \S+ \S+?(?:\[\d+\])?: `)
	dmesgLine     = regexp.MustCompile(`(?m)^\[\s*\d+\.\d+\] `)
	shellPrompt   = regexp.MustCompile(`(?m)^(\w[\w.-]*:(PRIMARY|SECONDARY|ARBITER|OTHER)> |\S+ \[direct: \w+\] \S+> |mongos> )`)
	foreignName   = regexp.MustCompile(`(?i)mysql|mariadb|innodb|postgres|pg_stat|ndb-status|my\.cnf`)
)

// shellOutputs are the files a collection script writes by running one shell command each.
var shellOutputs = map[string]bool{
	"rs_conf": true, "rs_status": true, "serverstatus": true, "host_info": true, "hostinfo": true,
	"rs_printreplicationinfo": true, "rs_printsecondaryreplicationinfo": true, "currentop": true,
	"getcmdlineopts": true, "buildinfo": true, "sh_status": true, "getparameter": true,
	"db_stats": true, "dbstats": true, "listdatabases": true, "ismaster": true, "hello": true,
	"replsetgetstatus": true, "replsetgetconfig": true, "balancer_status": true, "connpoolstats": true,
	"top": true, "rs_printslavereplicationinfo": true, "featurecompatibilityversion": true, "getlog": true, "collstats": true,
}

func classify(name string, head []byte) (Kind, string) {
	lower := strings.ToLower(name)
	stem := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(lower, ".gz"), ".out"), ".json")
	stem = strings.TrimSuffix(stem, ".txt")
	switch {
	case isFTDCHead(head):
		return FTDC, ""
	case isJSONLogHead(head):
		return LogJSON, ""
	case isCSVLogHead(head):
		return LogCSV, ""
	case legacyLogLine.Match(firstLines(head, 20)):
		return LogLegacy, ""
	}
	for key := range shellOutputs {
		if strings.ReplaceAll(stem, ".", "_") == key || strings.HasSuffix(strings.ReplaceAll(stem, ".", "_"), "_"+key) {
			return Shell, key
		}
	}
	if shellPrompt.Match(head) {
		return Shell, "transcript"
	}
	if sub := shellByContent(head); sub != "" {
		return Shell, sub
	}
	switch {
	case strings.HasPrefix(lower, "pt-mongodb-summary"), strings.HasPrefix(lower, "pt-summary"),
		strings.HasPrefix(lower, "mdiag"), strings.HasPrefix(lower, "getmongodata"):
		return Collector, stem
	case strings.HasPrefix(lower, "pbm"):
		return Collector, "pbm"
	case strings.HasPrefix(lower, "jrnl") || strings.HasPrefix(lower, "journal"):
		return OSLog, "journal"
	case strings.Contains(lower, "dmesg") || dmesgLine.Match(firstLines(head, 5)):
		return OSLog, "dmesg"
	case isMongoConfig(lower, head):
		return Config, ""
	case strings.HasPrefix(lower, "messages") || strings.HasPrefix(lower, "syslog") || strings.HasPrefix(lower, "kern"):
		if syslogLine.Match(firstLines(nonEmpty(head), 5)) {
			return OSLog, "syslog"
		}
	case foreignName.MatchString(lower):
		return Foreign, ""
	}
	if syslogLine.Match(firstLines(nonEmpty(head), 5)) {
		return OSLog, "syslog"
	}
	return Other, ""
}

// nonEmpty drops the blank lines a copied log often opens with.
func nonEmpty(head []byte) []byte {
	return bytes.TrimLeft(head, "\r\n\t ")
}

// shellByContent recognises the shell outputs people paste into a text file under any name.
func shellByContent(head []byte) string {
	has := func(words ...string) bool {
		for _, w := range words {
			if !bytes.Contains(head, []byte(w)) {
				return false
			}
		}
		return true
	}
	switch {
	case has("members", "stateStr", "optimes"):
		return "rs_status"
	case has("members", "priority", "votes", "protocolVersion"):
		return "rs_conf"
	case has("uptimeMillis", "opcounters", "connections"):
		return "serverstatus"
	case has("shardingVersion", "shards"):
		return "sh_status"
	}
	return ""
}

func isMongoConfig(lower string, head []byte) bool {
	named := strings.Contains(lower, "mongod.conf") || strings.Contains(lower, "mongos.conf") ||
		strings.HasSuffix(lower, "_config") || strings.HasSuffix(lower, ".conf") || strings.HasSuffix(lower, ".yaml")
	yaml := 0
	for _, key := range []string{"storage:", "net:", "systemLog:", "replication:", "sharding:", "processManagement:", "security:"} {
		if bytes.Contains(head, []byte(key)) {
			yaml++
		}
	}
	return named && yaml >= 2
}

func firstLines(head []byte, n int) []byte {
	end := 0
	for i := 0; i < n; i++ {
		next := bytes.IndexByte(head[end:], '\n')
		if next < 0 {
			return head
		}
		end += next + 1
	}
	return head[:end]
}

// isJSONLogHead: MongoDB's structured log, one JSON object per line opening with the timestamp.
// Some exports put a container or syslog prefix in front of the brace.
func isJSONLogHead(head []byte) bool {
	for _, line := range bytes.SplitN(head, []byte("\n"), 6) {
		brace := bytes.IndexByte(line, '{')
		if brace < 0 || brace > 512 {
			continue
		}
		line = line[brace:]
		if bytes.Contains(line, []byte(`"t":`)) && bytes.Contains(line, []byte(`"$date"`)) && bytes.Contains(line, []byte(`"msg"`)) {
			return true
		}
	}
	return false
}

// isCSVLogHead: a header row naming a time and a message column, and either a severity column or
// rows whose messages are MongoDB's "msg, attr={...}". Some exports keep nothing but the time and
// the message, and they are still the server's log.
func isCSVLogHead(head []byte) bool {
	line := head
	if end := bytes.IndexByte(head, '\n'); end >= 0 {
		line = head[:end]
	}
	lower := bytes.ToLower(line)
	if !bytes.Contains(lower, []byte("message")) || !(bytes.Contains(lower, []byte("timestamp")) || bytes.Contains(lower, []byte("time"))) ||
		!(bytes.Contains(line, []byte(";")) || bytes.Contains(line, []byte(","))) {
		return false
	}
	return bytes.Contains(lower, []byte("severity")) || bytes.Contains(head[len(line):], []byte(", attr={"))
}

// isFTDCHead: BSON documents back to back, each opening with its length and the "_id" the server
// stamps it with.
func isFTDCHead(head []byte) bool {
	if len(head) < 9 {
		return false
	}
	length := binary.LittleEndian.Uint32(head[:4])
	if length < 5 || length > 64<<20 {
		return false
	}
	return head[4] != 0x00 && bytes.HasPrefix(head[5:], []byte("_id\x00"))
}

// Describe is a one-line account of an entry for listings.
func (e *Entry) Describe() string {
	s := string(e.Kind)
	if e.Sub != "" {
		s += "/" + e.Sub
	}
	if e.Parts > 1 {
		s += fmt.Sprintf(" (joined from %d pieces)", e.Parts)
	}
	if e.Note != "" {
		s += " [" + e.Note + "]"
	}
	return s
}
