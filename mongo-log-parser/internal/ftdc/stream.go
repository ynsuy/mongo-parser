package ftdc

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// Stream is one node's diagnostic.data directory: rotations that continue each other in time.
// Directories are never merged with one another — that would splice two different servers together.
type Stream struct {
	Dir      string
	files    []*File
	Failures map[string]error  // files that could not be indexed, by path
	Partial  map[string]string // files indexed only up to the point they were cut short, by path
}

// OpenStream indexes every rotation in a directory. Indexing reads chunk headers only.
func OpenStream(dir string) (*Stream, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	return OpenFiles(dir, paths)
}

// Span is the time range the whole stream covers.
func (s *Stream) Span() (first, last time.Time) {
	first, _ = s.files[0].Span()
	_, last = s.files[len(s.files)-1].Span()
	return first, last
}

// Chunks counts the indexed chunks, which is what a window is selected from.
func (s *Stream) Chunks() int {
	total := 0
	for _, file := range s.files {
		total += len(file.chunks)
	}
	return total
}

type chunkRef struct {
	file  *File
	index int
	start time.Time
}

// Read decodes the chunks overlapping [from, to] across the whole stream, keeping the metrics the
// selector accepts. Whole chunks are returned; trimming to the exact window is the caller's job,
// because a rate needs the sample before the window to derive its first value.
// The done hook, when given, is called as each chunk is decoded, for a caller showing how far along
// it is. It runs on the decoding goroutines, so it has to be safe to call from several.
func (s *Stream) Read(from, to time.Time, want Selector, done func(chunks, total int)) (*Samples, error) {
	return decodeRefs(selectWindow(s.refs(), from, to), want, done)
}

// ReadLast decodes only the newest chunks, for a question about how things stood at the end.
func (s *Stream) ReadLast(chunks int, want Selector) (*Samples, error) {
	refs := s.refs()
	return decodeRefs(refs[max(0, len(refs)-chunks):], want, nil)
}

// Paths lists the metric names this stream carries in a window, by decoding one chunk of it.
//
// It answers a question a value lookup cannot: a metric that returns nothing may be a metric the
// server never recorded, or a name nobody ever collected. Reporting "no samples in this window" for
// the second reads as a flat line during an incident, which is a different and wrong story.
func (s *Stream) Paths(from, to time.Time) ([]string, error) {
	refs := selectWindow(s.refs(), from, to)
	if len(refs) == 0 {
		if refs = s.refs(); len(refs) == 0 {
			return nil, nil
		}
	}
	// One chunk is enough, but the first one may be the damaged one, and a node whose metric names
	// all vanished with a single bad chunk looks like a node that recorded nothing. Try the ones
	// after it, one at a time, so an undamaged stream still pays for exactly one chunk.
	var samples *Samples
	var err error
	for i := range min(len(refs), 8) {
		if samples, err = decodeRefs(refs[i:i+1], nil, nil); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(samples.Kinds))
	for path := range samples.Kinds {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

// refs is every indexed chunk in the stream, oldest first.
func (s *Stream) refs() []chunkRef {
	refs := make([]chunkRef, 0, 64)
	for _, file := range s.files {
		for i, chunk := range file.chunks {
			refs = append(refs, chunkRef{file, i, chunk.Start})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].start.Before(refs[j].start) })
	return refs
}

// selectWindow keeps the chunks a window touches. A chunk covers samples until the next one starts,
// so the chunk containing the window's first instant is kept even though it starts earlier.
func selectWindow(refs []chunkRef, from, to time.Time) []chunkRef {
	kept := make([]chunkRef, 0, len(refs))
	for i, ref := range refs {
		if !to.IsZero() && ref.start.After(to) {
			continue
		}
		if !from.IsZero() && i+1 < len(refs) && refs[i+1].start.Before(from) {
			continue
		}
		kept = append(kept, ref)
	}
	return kept
}

func decodeRefs(refs []chunkRef, want Selector, done func(chunks, total int)) (*Samples, error) {
	handles := map[string]*os.File{}
	defer func() {
		for _, handle := range handles {
			handle.Close()
		}
	}()
	for _, ref := range refs {
		if _, open := handles[ref.file.Path]; open {
			continue
		}
		handle, err := os.Open(ref.file.Path)
		if err != nil {
			return nil, err
		}
		handles[ref.file.Path] = handle
	}

	decoded := make([]*chunkData, len(refs))
	masks := &keepMasks{}
	failures := make([]error, len(refs))
	limit := make(chan struct{}, max(1, min(4, runtime.NumCPU())))
	var finished atomic.Int64
	var wait sync.WaitGroup
	for slot, ref := range refs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			defer func() {
				if done != nil {
					done(int(finished.Add(1)), len(refs))
				}
			}()
			payload, err := ref.file.payloadAt(handles[ref.file.Path], ref.index)
			if err != nil {
				failures[slot] = err
				return
			}
			decoded[slot], failures[slot] = decodeChunk(payload, want, masks)
		}()
	}
	wait.Wait()

	samples := &Samples{Values: map[string][]float64{}, Kinds: map[string]string{}}
	var firstFailure error
	total := 0
	for i, chunk := range decoded {
		if failures[i] != nil {
			samples.damaged(refs[i].file.Path, failures[i])
			if firstFailure == nil {
				firstFailure = fmt.Errorf("%s: %w", refs[i].file.Path, failures[i])
			}
			continue
		}
		total += len(chunk.timestamps)
	}
	// Every series is made once at its full length: grown chunk by chunk, a week of samples was
	// copied over and over on its way there.
	samples.Timestamps = make([]int64, 0, total)
	for i, chunk := range decoded {
		if failures[i] != nil {
			continue
		}
		offset := len(samples.Timestamps)
		samples.Timestamps = append(samples.Timestamps, chunk.timestamps...)
		for path, values := range chunk.values {
			series, seen := samples.Values[path]
			if !seen {
				series = make([]float64, total)
				for j := range series {
					series[j] = math.NaN()
				}
				samples.Values[path] = series
			}
			copy(series[offset:], values)
			samples.Kinds[path] = chunk.kinds[path]
		}
		decoded[i] = nil
	}
	// Every chunk gone is a different answer from some of them gone: there is nothing to draw and
	// nothing to say about the window, so the caller hears why rather than seeing an empty chart.
	if len(refs) > 0 && samples.Skipped == len(refs) {
		return nil, firstFailure
	}
	return samples, nil
}

// Host is the server this stream's data came from, as the server itself recorded it.
//
// FTDC writes serverStatus.host into every chunk's reference document, which identifies the node
// far better than the directory someone happened to unpack it into: two nodes can be unpacked side
// by side under one folder, and a folder can be renamed. Empty when the bundle does not say.
func (s *Stream) Host() string {
	reference, err := s.Reference()
	if err != nil {
		return ""
	}
	return hostOf(reference)
}

// hostOf reads the server name out of a reference document, trying the places MongoDB records it.
// lookup finds a field in a reference document, inside 8.0's common/ or shard/ section when the
// sample is nested that way (see serviceSections).
func lookup(reference bson.Raw, path ...string) (bson.RawValue, error) {
	value, err := reference.LookupErr(path...)
	if err == nil {
		return value, nil
	}
	for _, section := range serviceSections {
		nested := append([]string{strings.TrimSuffix(section, "/")}, path...)
		if found, nestedErr := reference.LookupErr(nested...); nestedErr == nil {
			return found, nil
		}
	}
	return value, err
}

func hostOf(reference bson.Raw) string {
	for _, path := range [][]string{
		{"serverStatus", "host"},
		{"hostInfo", "system", "hostname"},
		{"serverStatus", "repl", "me"},
	} {
		value, err := lookup(reference, path...)
		if err != nil {
			continue
		}
		if host, ok := value.StringValueOK(); ok && host != "" {
			return host
		}
	}
	return ""
}

// Reference is the reference document of the stream's first chunk: the whole serverStatus and
// hostInfo snapshot the deltas are measured against, strings and all.
//
// The metric columns keep only numbers, so anything that says which server this is lives here.
func (s *Stream) Reference() (bson.Raw, error) {
	refs := s.refs()
	if len(refs) == 0 {
		return nil, fmt.Errorf("%s: no chunks", s.Dir)
	}
	// The first chunk normally answers. When it is damaged, the ones after it carry the same
	// reference document, and losing a node's name to one bad chunk would scatter its logs and its
	// metrics into two nodes that cannot be compared.
	var err error
	for _, ref := range refs[:min(len(refs), 8)] {
		var reference bson.Raw
		reference, err = referenceAt(ref)
		if err == nil {
			return reference, nil
		}
	}
	return nil, err
}

func referenceAt(ref chunkRef) (bson.Raw, error) {
	handle, err := os.Open(ref.file.Path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	payload, err := ref.file.payloadAt(handle, ref.index)
	if err != nil {
		return nil, err
	}
	return referenceOf(payload)
}

// OpenFiles indexes a chosen set of files, for diagnostic data that sits beside other things.
func OpenFiles(dir string, paths []string) (*Stream, error) {
	stream := &Stream{Dir: dir, Failures: map[string]error{}, Partial: map[string]string{}}
	for _, path := range paths {
		file, err := Open(path)
		if err != nil {
			stream.Failures[path] = err
			continue
		}
		if file.Cut != "" {
			stream.Partial[path] = file.Cut
		}
		stream.files = append(stream.files, file)
	}
	if len(stream.files) == 0 {
		return nil, fmt.Errorf("%s: no readable FTDC files", dir)
	}
	sort.Slice(stream.files, func(i, j int) bool {
		left, _ := stream.files[i].Span()
		right, _ := stream.files[j].Span()
		return left.Before(right)
	})
	return stream, nil
}

// Files counts the files the stream was built from.
func (s *Stream) Files() int { return len(s.files) }

// Identity is what the server said about itself in the stream's first reference document.
type Identity struct {
	Host    string `json:"host,omitempty"`    // serverStatus.host: the machine's name and port
	Me      string `json:"me,omitempty"`      // serverStatus.repl.me: its name in the replica set config
	SetName string `json:"setName,omitempty"` // serverStatus.repl.setName
	Process string `json:"process,omitempty"` // mongod or mongos
	Version string `json:"version,omitempty"`
}

func (s *Stream) Identity() Identity {
	reference, err := s.Reference()
	if err != nil {
		return Identity{}
	}
	text := func(path ...string) string {
		value, err := lookup(reference, path...)
		if err != nil {
			return ""
		}
		out, _ := value.StringValueOK()
		return out
	}
	id := Identity{
		Host:    text("serverStatus", "host"),
		Me:      text("serverStatus", "repl", "me"),
		SetName: text("serverStatus", "repl", "setName"),
		Process: text("serverStatus", "process"),
		Version: text("serverStatus", "version"),
	}
	if id.Host == "" {
		id.Host = hostOf(reference)
	}
	return id
}
