// Package ingest reads a bundle once into the store: every log line verbatim in compressed blocks,
// every FTDC stream indexed, every shell output and OS log kept whole, and every stream tied to the
// node that wrote it.
package ingest

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mongo-log-parser/internal/ftdc"
	"mongo-log-parser/internal/source"
	"mongo-log-parser/internal/store"
)

// Fingerprint identifies the bundle's contents cheaply: every file's path, size and modification
// time. A store built from another fingerprint is out of date.
func Fingerprint(root string) (string, error) {
	h := sha256.New()
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		fmt.Fprintf(h, "%s|%d|%d\n", root, info.Size(), info.ModTime().UnixNano())
		return hex.EncodeToString(h.Sum(nil))[:16], nil
	}
	var lines []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			lines = append(lines, fmt.Sprintf("%s|%d|%d", p, fi.Size(), fi.ModTime().UnixNano()))
		}
		return nil
	})
	sort.Strings(lines)
	for _, line := range lines {
		fmt.Fprintln(h, line)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// Progress is told what is happening, for a progress line on stderr.
type Progress func(done, total int, what string)

// maxArtifact caps what is kept of one non-log file. An OS log can be large; its head is still
// enough to be read, and the cut is recorded.
const maxArtifact = 32 << 20

type ingester struct {
	root    string
	st      *store.Store
	tmp     string
	ftdcDir string

	ids struct{ source, stream, block atomic.Int64 }

	mu      sync.Mutex
	sources []sourceRow
	streams []streamRow
	ftdc    map[string]*ftdcGroup // by archive-qualified directory
	errors  []string

	// Bundles carry the same file twice more often than not: an archive and its unpacked copy, the
	// same log attached to two emails. A copy is read once; counted twice it doubles every number.
	seenContent map[string]string // content hash -> path of the first copy
	seenFTDC    map[string]string // file name and size -> path of the first copy
	dupStreams  map[int64]bool

	blocks chan blockOut
}

type sourceRow struct {
	ID        int64
	Path      string
	Container string
	Kind      string
	Size      int64
	Note      string
	Artifact  *string
	Sub       string
}

type streamRow struct {
	ID       int64
	Source   int64
	Instance string
	Dir      string // archive-qualified directory of its source, for fallback grouping
	Name     string // its source's file name
	Stats    Stats
	built    *builder
}

type ftdcGroup struct {
	Origin string   // the bundle path of the directory
	Files  []string // on-disk paths, extracted where they came out of an archive
}

// Run builds the store for root from scratch.
func Run(root, dir string, progress Progress) (*store.Store, error) {
	print, err := Fingerprint(root)
	if err != nil {
		return nil, err
	}
	st, err := store.Create(dir)
	if err != nil {
		return nil, err
	}
	in := &ingester{root: root, st: st, tmp: filepath.Join(dir, "tmp"), ftdcDir: filepath.Join(dir, "ftdc"),
		ftdc: map[string]*ftdcGroup{}, blocks: make(chan blockOut, 64),
		seenContent: map[string]string{}, seenFTDC: map[string]string{}, dupStreams: map[int64]bool{}}
	_ = os.MkdirAll(in.tmp, 0o755)
	_ = os.MkdirAll(in.ftdcDir, 0o755)

	units, err := source.TopLevel(root)
	if err != nil {
		return nil, err
	}

	writeErr := make(chan error, 1)
	go func() { writeErr <- in.writeBlocks() }()

	started := time.Now()
	var done atomic.Int64
	work := make(chan []string)
	var wg sync.WaitGroup
	workers := runtime.NumCPU()
	if workers > 10 {
		workers = 10
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for unit := range work {
				if err := source.Walk(root, unit, in.tmp, in.visit); err != nil {
					in.mu.Lock()
					in.errors = append(in.errors, fmt.Sprintf("%s: %v", unit[0], err))
					in.mu.Unlock()
				}
				n := done.Add(1)
				if progress != nil {
					progress(int(n), len(units), filepath.Base(unit[0]))
				}
			}
		}()
	}
	for _, unit := range units {
		work <- unit
	}
	close(work)
	wg.Wait()
	close(in.blocks)
	if err := <-writeErr; err != nil {
		return nil, err
	}
	_ = os.RemoveAll(in.tmp)
	if err := in.dropCopies(); err != nil {
		return nil, err
	}

	if progress != nil {
		progress(len(units), len(units), "indexing diagnostic data")
	}
	ftdcRows := in.indexFTDC()
	if progress != nil {
		progress(len(units), len(units), "tying streams to nodes")
	}
	nodes, err := in.writeCatalogue(ftdcRows)
	if err != nil {
		return nil, err
	}
	if err := st.BuildIndexes(); err != nil {
		return nil, err
	}
	if err := in.writeSilences(nodes); err != nil {
		return nil, err
	}
	errs, _ := json.Marshal(in.errors)
	meta := map[string]string{
		"root": root, "fingerprint": print, "ingested_at": time.Now().UTC().Format(time.RFC3339),
		"ingest_seconds": fmt.Sprintf("%.1f", time.Since(started).Seconds()), "errors": string(errs),
	}
	if err := st.Finish(meta); err != nil {
		return nil, err
	}
	return st, nil
}

func (in *ingester) addSource(e *source.Entry) int64 {
	id := in.ids.source.Add(1)
	in.mu.Lock()
	in.sources = append(in.sources, sourceRow{ID: id, Path: e.Path, Container: e.Container, Kind: string(e.Kind),
		Size: e.Size, Note: e.Note, Sub: e.Sub})
	in.mu.Unlock()
	return id
}

// visit is called once per file of the bundle, from several workers at once.
func (in *ingester) visit(e *source.Entry, content io.Reader) error {
	sourceID := in.addSource(e)
	switch e.Kind {
	case source.LogJSON:
		id := in.ids.stream.Add(1)
		b := in.newBuilder(id)
		hash := sha256.New()
		stats, err := ReadJSONLog(io.TeeReader(content, hash), b.add)
		b.flush()
		in.mu.Lock()
		if earlier := in.firstCopy(hash.Sum(nil), e.Path); earlier != "" {
			in.dupStreams[id] = true
			in.setNote(sourceID, "an exact copy of "+earlier+"; read once")
		} else {
			in.streams = append(in.streams, streamRow{ID: id, Source: sourceID, Dir: e.Dir, Name: e.Name, Stats: stats, built: b})
		}
		in.mu.Unlock()
		return err

	case source.LogCSV:
		builders := map[string]*builder{}
		hash := sha256.New()
		streams, note, err := ReadCSVLog(io.TeeReader(content, hash), func(instance string, rec store.Record) {
			b, ok := builders[instance]
			if !ok {
				b = in.newBuilder(in.ids.stream.Add(1))
				builders[instance] = b
			}
			b.add(rec)
		})
		for _, b := range builders {
			b.flush()
		}
		in.mu.Lock()
		defer in.mu.Unlock()
		if earlier := in.firstCopy(hash.Sum(nil), e.Path); earlier != "" {
			for _, b := range builders {
				in.dupStreams[b.stream] = true
			}
			in.setNote(sourceID, "an exact copy of "+earlier+"; read once")
			return err
		}
		for instance, s := range streams {
			b, ok := builders[instance]
			if !ok {
				// Only unreadable rows were seen for it; it still gets a stream, so they are counted.
				b = in.newBuilder(in.ids.stream.Add(1))
			}
			in.streams = append(in.streams, streamRow{ID: b.stream, Source: sourceID, Instance: instance, Dir: e.Dir, Name: e.Name, Stats: s.Stats, built: b})
		}
		in.setNote(sourceID, note)
		return err

	case source.FTDC:
		key := fmt.Sprintf("%s|%d", e.Name, e.Size)
		in.mu.Lock()
		earlier, copied := in.seenFTDC[key]
		if !copied {
			in.seenFTDC[key] = e.Path
		} else {
			in.setNote(sourceID, "an exact copy of "+earlier+"; read once")
		}
		in.mu.Unlock()
		if copied {
			_, _ = io.Copy(io.Discard, content)
			return nil
		}
		path := e.OnDisk
		if path == "" {
			// Out of an archive: written to disk, because chunks are read by offset.
			sum := sha256.Sum256([]byte(e.Dir))
			dir := filepath.Join(in.ftdcDir, hex.EncodeToString(sum[:])[:12])
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			path = filepath.Join(dir, e.Name)
			f, err := os.Create(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, content)
			f.Close()
			if err != nil {
				return err
			}
		}
		in.mu.Lock()
		group, ok := in.ftdc[e.Dir]
		if !ok {
			group = &ftdcGroup{Origin: e.Dir}
			in.ftdc[e.Dir] = group
		}
		group.Files = append(group.Files, path)
		in.mu.Unlock()
		return nil

	case source.Shell, source.OSLog, source.Collector, source.Config, source.LogLegacy:
		data, err := io.ReadAll(io.LimitReader(content, maxArtifact+1))
		if err != nil {
			return err
		}
		note := ""
		if len(data) > maxArtifact {
			data = data[:maxArtifact]
			note = fmt.Sprintf("kept the first %d MiB only", maxArtifact>>20)
		}
		text := string(data)
		sum := sha256.Sum256(data)
		in.mu.Lock()
		defer in.mu.Unlock()
		if earlier := in.firstCopy(sum[:], e.Path); earlier != "" {
			in.setNote(sourceID, "an exact copy of "+earlier+"; kept once")
			return nil
		}
		for i := range in.sources {
			if in.sources[i].ID == sourceID {
				in.sources[i].Artifact = &text
				if note != "" {
					in.sources[i].Note = joinNote(in.sources[i].Note, note)
				}
			}
		}
		return nil
	}
	_, _ = io.Copy(io.Discard, content)
	return nil
}

// firstCopy registers content seen at path and reports the earlier path when it is a copy.
// Must be called with mu held.
func (in *ingester) firstCopy(sum []byte, path string) string {
	key := hex.EncodeToString(sum)
	if earlier, ok := in.seenContent[key]; ok {
		return earlier
	}
	in.seenContent[key] = path
	return ""
}

// setNote must be called with mu held.
func (in *ingester) setNote(sourceID int64, note string) {
	if note == "" {
		return
	}
	for i := range in.sources {
		if in.sources[i].ID == sourceID {
			in.sources[i].Note = joinNote(in.sources[i].Note, note)
		}
	}
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

const insertBlock = `INSERT INTO block(id, stream, first_ts, last_ts, lines, sev_mask, ids, data) VALUES (?,?,?,?,?,?,?,?)`

// writeBlocks is the only writer: SQLite takes one at a time, and the compressing has already been
// done by the workers.
func (in *ingester) writeBlocks() error {
	tx, err := in.st.DB.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(insertBlock)
	if err != nil {
		return err
	}
	written := 0
	for b := range in.blocks {
		m := b.meta
		if _, err := stmt.Exec(m.ID, m.Stream, m.First, m.Last, m.Lines, m.SevMask, b.ids, b.data); err != nil {
			return err
		}
		if written++; written%2000 == 0 {
			if err := tx.Commit(); err != nil {
				return err
			}
			if tx, err = in.st.DB.Begin(); err != nil {
				return err
			}
			if stmt, err = tx.Prepare(insertBlock); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (in *ingester) dropCopies() error {
	if len(in.dupStreams) == 0 {
		return nil
	}
	ids := make([]string, 0, len(in.dupStreams))
	for id := range in.dupStreams {
		ids = append(ids, fmt.Sprint(id))
	}
	_, err := in.st.DB.Exec(`DELETE FROM block WHERE stream IN (` + strings.Join(ids, ",") + `)`)
	return err
}

// writeSilences records, per node, every stretch of at least store.SilenceMinutes with no log line.
// A node's files are laid end to end: the gaps inside each file, unless another file of the node
// covers them, and the gaps between one file's last line and the next one's first.
func (in *ingester) writeSilences(nodes []*nodeRow) error {
	threshold := int64(store.SilenceMinutes * 60 * 1000)
	type silence struct {
		node          int64
		from, to      int64
		before, after store.Ref
	}
	var found []silence
	for _, n := range nodes {
		var streams []*streamRow
		for _, it := range n.members {
			if it.stream != nil && it.stream.Stats.Lines > 0 && it.stream.built != nil && it.stream.built.has {
				streams = append(streams, it.stream)
			}
		}
		sort.Slice(streams, func(i, j int) bool { return streams[i].Stats.First < streams[j].Stats.First })
		covered := func(self *streamRow, from, to int64) bool {
			for _, o := range streams {
				if o != self && o.Stats.First < to && o.Stats.Last > from {
					return true
				}
			}
			return false
		}
		var end int64
		var endRef store.Ref
		for i, s := range streams {
			b := s.built
			if i > 0 && s.Stats.First-end >= threshold {
				found = append(found, silence{n.ID, end, s.Stats.First, endRef, b.first})
			}
			for _, g := range b.gaps {
				if !covered(s, g.From, g.To) {
					found = append(found, silence{n.ID, g.From, g.To, g.Before, g.After})
				}
			}
			if s.Stats.Last > end {
				end, endRef = s.Stats.Last, b.lastRef
			}
		}
	}
	for _, s := range found {
		var others sql.NullInt64
		_ = in.st.DB.QueryRow(`SELECT sum(b.lines) FROM block b JOIN stream st ON st.id = b.stream
			WHERE st.node != ? AND b.last_ts > ? AND b.first_ts < ?`, s.node, s.from, s.to).Scan(&others)
		if _, err := in.st.DB.Exec(`INSERT INTO silence(node, from_ts, to_ts, others, before_block, before_slot, after_block, after_slot)
			VALUES (?,?,?,?,?,?,?,?)`, s.node, s.from, s.to, others.Int64, s.before.Block, s.before.Slot, s.after.Block, s.after.Slot); err != nil {
			return err
		}
	}
	return nil
}

type ftdcRow struct {
	Origin   string
	Dir      string
	Identity ftdc.Identity
	First    int64
	Last     int64
	Chunks   int
	Files    int
	Note     string
}

// indexFTDC opens each diagnostic data stream far enough to know its span and whose it is.
func (in *ingester) indexFTDC() []ftdcRow {
	var out []ftdcRow
	keys := make([]string, 0, len(in.ftdc))
	for key := range in.ftdc {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := in.ftdc[key]
		row := ftdcRow{Origin: group.Origin, Dir: filepath.Dir(group.Files[0])}
		stream, err := ftdc.OpenFiles(row.Dir, group.Files)
		if err != nil {
			row.Note = err.Error()
			row.Files = len(group.Files)
			out = append(out, row)
			continue
		}
		first, last := stream.Span()
		row.First, row.Last = first.UnixMilli(), last.UnixMilli()
		row.Chunks, row.Files = stream.Chunks(), stream.Files()
		row.Identity = stream.Identity()
		var notes []string
		if n := len(stream.Failures); n > 0 {
			var names []string
			for path, ferr := range stream.Failures {
				reason := strings.ReplaceAll(ferr.Error(), path+": ", "")
				names = append(names, fmt.Sprintf("%s (%s)", filepath.Base(path), reason))
			}
			sort.Strings(names)
			notes = append(notes, fmt.Sprintf("%d of its files could not be read: %s", n, strings.Join(names, "; ")))
		}
		if n := len(stream.Partial); n > 0 {
			var names []string
			for path, why := range stream.Partial {
				names = append(names, fmt.Sprintf("%s (%s)", filepath.Base(path), why))
			}
			sort.Strings(names)
			was := "was read only as far as it goes"
			if n > 1 {
				was = "were read only as far as they go"
			}
			notes = append(notes, fmt.Sprintf("%d of its files %s: %s", n, was, strings.Join(names, "; ")))
		}
		row.Note = strings.Join(notes, "; ")
		out = append(out, row)
	}
	return out
}
