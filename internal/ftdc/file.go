package ftdc

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
)

// ChunkInfo locates one chunk and says when its first sample was taken.
type ChunkInfo struct {
	Start  time.Time `json:"start"`
	Offset int64     `json:"offset"`
	Length int       `json:"length"`
	Process
}

// Process is what a chunk's reference document says about the process and the machine it ran on.
// The uptime tells one run of a process from the next even where no log line of the restart
// survived, and a run is the only span over which a version, a cache size or a machine's memory
// can be read as one value.
type Process struct {
	UpMillis   int64  `json:"up,omitempty"` // how long the process had been up at the chunk's start
	Version    string `json:"version,omitempty"`
	CacheBytes int64  `json:"cache,omitempty"` // WiredTiger's configured cache
	MemKB      int64  `json:"mem_kb,omitempty"`
	CPUs       int64  `json:"cpus,omitempty"`
}

// File is an indexed FTDC file: chunks located and their start times known, deltas untouched.
// The bytes are not retained — a bundle holds gigabytes of captures — so a decode reads back only
// the chunks a window asks for.
type File struct {
	Path   string
	Cut    string // set when the file ends in the middle of a document: what was indexed, and what was lost
	chunks []ChunkInfo
	last   time.Time // when its last sample was taken
}

// Open indexes a file, reusing the on-disk index while the file is unchanged. Building an index
// inflates only each chunk's reference document, a fraction of what a full decode costs.
func Open(path string) (*File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if cached := loadIndex(path, info); cached != nil && len(cached.Chunks) > 0 {
		return &File{Path: path, chunks: cached.Chunks, last: cached.Last}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	file := &File{Path: path}
	// A capture can be damaged in two ways, and they need different answers.
	//
	// One that was interrupted — the node went away, or the copy out of it stopped — ends in the
	// middle of a document. Everything before that point is whole and is hours of a node's history,
	// so the file is indexed up to the damage and says where it stopped, rather than being refused
	// entire.
	//
	// The other is damaged in the middle and whole again after it. Stopping at the first bad
	// document then throws away everything that follows, all of it behind a single unreadable
	// stretch. So a bad document is skipped rather than obeyed: the scan moves forward to the next
	// offset where a whole FTDC document begins, says how much it stepped over, and carries on. Only
	// when nothing whole follows is the rest of the file given up.
	//
	// Nothing indexed at all means there is no history to keep, and that is an error.
	var damage []string
	tailLost := false
	for position := 0; position < len(data); {
		size, kind, ok := documentAt(data, position, false)
		if !ok {
			next, found := resume(data, position)
			if !found {
				damage = append(damage, fmt.Sprintf("%d bytes at its end were not read", len(data)-position))
				tailLost = true
				break
			}
			damage = append(damage, fmt.Sprintf("%d unreadable bytes at offset %d were skipped", next-position, position))
			position = next
			continue
		}
		document := bson.Raw(data[position : position+size])
		offset := position
		position += size
		if kind != 1 {
			continue // metadata, and the periodic metadata newer servers write: nothing to index.
		}
		_, payload, hasPayload := document.Lookup("data").BinaryOK()
		if !hasPayload {
			damage = append(damage, fmt.Sprintf("a chunk at offset %d had no payload and was skipped", offset))
			continue
		}
		chunk := ChunkInfo{Offset: int64(offset), Length: size}
		if reference, err := referenceOf(payload); err == nil {
			if when, ok := chunkStart(reference); ok {
				chunk.Start = when
			}
			chunk.Process = processOf(reference)
		}
		if chunk.Start.IsZero() {
			// Older captures can lack a clock column; the envelope id is the collector's own stamp.
			if millis, ok := document.Lookup("_id").DateTimeOK(); ok {
				chunk.Start = time.UnixMilli(millis).UTC()
			}
		}
		file.chunks = append(file.chunks, chunk)
	}
	if len(file.chunks) == 0 {
		if len(damage) > 0 {
			return nil, fmt.Errorf("%s: no whole FTDC chunk (%s)", path, strings.Join(damage, "; "))
		}
		return nil, fmt.Errorf("%s: no FTDC chunks", path)
	}
	file.last = file.lastSample()
	if len(damage) > 0 {
		// Not cached: the index on disk holds chunks and not damage, and a file that is damaged
		// should say so every time it is read rather than only the first.
		summary := fmt.Sprintf("%d chunks read", len(file.chunks))
		if tailLost {
			summary = fmt.Sprintf("cut short after %d chunks", len(file.chunks))
		}
		file.Cut = summary + "; " + strings.Join(damage, "; ")
		return file, nil
	}
	saveIndex(path, info, file.chunks, file.last)
	return file, nil
}

// lastSample is when the file's last sample was taken. The index knows when each chunk began, and
// a chunk can hold five minutes of samples, so the last chunk is decoded for the time it ended.
// A last chunk that cannot be decoded ends, as far as anyone can tell, where it began.
func (f *File) lastSample() time.Time {
	i := len(f.chunks) - 1
	samples, err := decodeRefs([]chunkRef{{f, i, f.chunks[i].Start}}, func(string) bool { return false }, nil)
	if err != nil || len(samples.Timestamps) == 0 {
		return f.chunks[i].Start
	}
	return time.UnixMilli(samples.Timestamps[len(samples.Timestamps)-1]).UTC()
}

// minDocument is the smallest FTDC envelope worth recognising when resynchronising: an _id, a type
// and a payload cannot be written in fewer bytes, and a lower bar lets a stretch of garbage that
// happens to begin with a small length be mistaken for a document.
const minDocument = 32

// documentAt reports whether a whole, valid FTDC document begins at position: its length fits in
// the file and the bytes validate as BSON.
//
// strict also demands an integer type of 0, 1 or 2, and that is the test resume uses to decide
// where readable data starts again: it is a signature random bytes do not pass. Reading forward
// through a file that is not damaged, a whole document without a type is accepted and skipped
// (kind -1) as it always was — older servers wrote metadata that way, so treating it as damage
// would only ever be wrong.
func documentAt(data []byte, position int, strict bool) (size int, kind int32, ok bool) {
	if len(data)-position < 4 {
		return 0, 0, false
	}
	size = int(binary.LittleEndian.Uint32(data[position:]))
	if size < minDocument || size > len(data)-position {
		return 0, 0, false
	}
	document := bson.Raw(data[position : position+size])
	if document.Validate() != nil {
		return 0, 0, false
	}
	value, err := document.LookupErr("type")
	if err != nil {
		if strict {
			return 0, 0, false
		}
		return size, -1, true
	}
	kind, isInt := value.Int32OK()
	if !isInt || kind < 0 || kind > 2 {
		return 0, 0, false
	}
	return size, kind, true
}

// resume finds the next offset after a damaged one where a whole FTDC document begins.
func resume(data []byte, from int) (int, bool) {
	for position := from + 1; position+4 <= len(data); position++ {
		if _, _, ok := documentAt(data, position, true); ok {
			return position, true
		}
	}
	return 0, false
}

// Chunks reports the index, oldest first.
func (f *File) Chunks() []ChunkInfo { return f.chunks }

// Span is the time range the file covers, from its first sample to its last.
func (f *File) Span() (first, last time.Time) {
	last = f.last
	if last.IsZero() {
		last = f.chunks[len(f.chunks)-1].Start
	}
	return f.chunks[0].Start, last
}

// Read decodes the chunks overlapping [from, to] and keeps the metrics the selector accepts.
// Zero times mean "no bound". Whole chunks are returned: trimming to the exact window is left to
// the caller, because a rate needs the sample immediately before the window to derive its first value.
func (f *File) Read(from, to time.Time, want Selector) (*Samples, error) {
	refs := make([]chunkRef, len(f.chunks))
	for i, chunk := range f.chunks {
		refs[i] = chunkRef{f, i, chunk.Start}
	}
	return decodeRefs(selectWindow(refs, from, to), want, nil)
}

// payloadAt reads one chunk's compressed bytes back from disk.
func (f *File) payloadAt(handle *os.File, index int) ([]byte, error) {
	chunk := f.chunks[index]
	raw := make([]byte, chunk.Length)
	if _, err := handle.ReadAt(raw, chunk.Offset); err != nil && err != io.EOF {
		return nil, err
	}
	_, payload, ok := bson.Raw(raw).Lookup("data").BinaryOK()
	if !ok {
		return nil, fmt.Errorf("%s: chunk without binary payload", f.Path)
	}
	return payload, nil
}

func chunkStart(reference bson.Raw) (time.Time, bool) {
	for _, path := range clockPaths {
		value, err := lookup(reference, splitPath(path)...)
		if err != nil {
			continue
		}
		switch value.Type {
		case bsontype.DateTime:
			if millis, ok := value.DateTimeOK(); ok {
				return time.UnixMilli(millis).UTC(), true
			}
		case bsontype.Int64:
			if millis, ok := value.Int64OK(); ok {
				return time.UnixMilli(millis).UTC(), true
			}
		}
	}
	return time.Time{}, false
}

func processOf(reference bson.Raw) Process {
	number := func(path ...string) int64 {
		value, err := lookup(reference, path...)
		if err != nil {
			return 0
		}
		if n, ok := value.AsInt64OK(); ok {
			return n
		}
		if f, ok := value.DoubleOK(); ok {
			return int64(f)
		}
		return 0
	}
	p := Process{
		UpMillis:   number("serverStatus", "uptimeMillis"),
		CacheBytes: number("serverStatus", "wiredTiger", "cache", "maximum bytes configured"),
		MemKB:      number("systemMetrics", "memory", "MemTotal_kb"),
		CPUs:       number("systemMetrics", "cpu", "num_logical_cores"),
	}
	if p.UpMillis == 0 {
		p.UpMillis = number("serverStatus", "uptime") * 1000
	}
	if value, err := lookup(reference, "serverStatus", "version"); err == nil {
		p.Version, _ = value.StringValueOK()
	}
	return p
}

func splitPath(path string) []string {
	parts := make([]string, 0, 3)
	for start := 0; start <= len(path); {
		end := start
		for end < len(path) && path[end] != '/' {
			end++
		}
		parts = append(parts, path[start:end])
		start = end + 1
	}
	return parts
}
