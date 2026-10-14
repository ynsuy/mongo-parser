package store

import (
	"container/heap"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Log lines are kept in blocks: a thousand or so consecutive lines of one stream, each with a small
// parsed header (time, severity, component, id, context, message) beside the line exactly as it
// was written, compressed together. A JSON log compresses ten to one this way, because its keys
// repeat on every line; a row per line in a table did not compress at all, and the store came out
// many times larger than the bundle it was read from.
//
// What a query can be narrowed by without opening a block — its stream, its time range, which
// severities and which log ids it holds — sits in the block table. Everything else is decided by
// opening the blocks that survive and reading their headers, which costs far less than parsing
// JSON: the raw line is only parsed when an attribute is asked about.

// Record is one log line.
type Record struct {
	Stream    int64
	Block     int64
	Slot      int
	Line      int
	TS        int64
	Sev       string
	Comp      string
	LogID     int
	Ctx       string
	Msg       string
	Truncated bool
	attr      string // the attributes as JSON when they are not inside Raw (an export's rows)
	Raw       string

	// The attributes cut out of Raw, once: several analyzers read every line's.
	attrRaw  json.RawMessage
	attrRead bool
}

// Attr is the line's attributes as JSON, or nil.
func (r *Record) Attr() json.RawMessage {
	if r.attr != "" {
		return json.RawMessage(r.attr)
	}
	if !r.attrRead {
		r.attrRaw, r.attrRead = r.readAttr(), true
	}
	return r.attrRaw
}

func (r *Record) readAttr() json.RawMessage {
	start := strings.IndexByte(r.Raw, '{')
	if start < 0 {
		return nil
	}
	if value, found, ok := Field(r.Raw[start:], "attr"); ok {
		if !found {
			return nil
		}
		return json.RawMessage(value)
	}
	var line struct {
		Attr json.RawMessage `json:"attr"`
	}
	if json.Unmarshal([]byte(r.Raw[start:]), &line) != nil {
		return nil
	}
	return line.Attr
}

// SetAttr records attributes that are not inside the raw line.
func (r *Record) SetAttr(attr string) { r.attr = attr }

// HasSeparateAttr reports whether the attributes were stored apart from the raw line.
func (r *Record) HasSeparateAttr() bool { return r.attr != "" }

// Ref names a line by where it is stored.
type Ref struct {
	Block int64 `json:"block"`
	Slot  int   `json:"slot"`
}

var severities = []string{"F", "E", "W", "I", "D1", "D2", "D3", "D4", "D5", "D"}

// SevBit is a severity's bit in a block's mask; anything unrecognised shares the last one.
func SevBit(sev string) int64 {
	for i, s := range severities {
		if s == sev {
			return 1 << i
		}
	}
	return 1 << len(severities)
}

// BlockMeta is what the block table holds besides the data.
type BlockMeta struct {
	ID      int64
	Stream  int64
	First   int64
	Last    int64
	Lines   int
	SevMask int64
	IDs     []int
}

var (
	encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	decoder, _ = zstd.NewReader(nil)
)

// EncodeBlock lays records out and compresses them.
func EncodeBlock(records []Record) []byte {
	var buf []byte
	var tmp [binary.MaxVarintLen64]byte
	putInt := func(v int64) { buf = append(buf, tmp[:binary.PutVarint(tmp[:], v)]...) }
	putStr := func(s string) { putInt(int64(len(s))); buf = append(buf, s...) }
	base := int64(0)
	if len(records) > 0 {
		base = records[0].TS
	}
	putInt(base)
	putInt(int64(len(records)))
	for i := range records {
		r := &records[i]
		putInt(int64(r.Line))
		putInt(r.TS - base)
		putStr(r.Sev)
		putStr(r.Comp)
		putInt(int64(r.LogID))
		putStr(r.Ctx)
		putStr(r.Msg)
		flags := int64(0)
		if r.Truncated {
			flags |= 1
		}
		putInt(flags)
		putStr(r.attr)
		putStr(r.Raw)
	}
	return encoder.EncodeAll(buf, nil)
}

// DecodeBlock is EncodeBlock's inverse.
func DecodeBlock(id, stream int64, data []byte) ([]Record, error) {
	buf, err := decoder.DecodeAll(data, nil)
	if err != nil {
		return nil, err
	}
	pos := 0
	bad := errors.New("corrupt block")
	getInt := func() (int64, error) {
		v, n := binary.Varint(buf[pos:])
		if n <= 0 {
			return 0, bad
		}
		pos += n
		return v, nil
	}
	getStr := func() (string, error) {
		n, err := getInt()
		if err != nil || n < 0 || pos+int(n) > len(buf) {
			return "", bad
		}
		s := string(buf[pos : pos+int(n)])
		pos += int(n)
		return s, nil
	}
	base, err := getInt()
	if err != nil {
		return nil, err
	}
	count, err := getInt()
	if err != nil {
		return nil, err
	}
	records := make([]Record, count)
	for i := range records {
		r := &records[i]
		r.Stream, r.Block, r.Slot = stream, id, i
		var v int64
		if v, err = getInt(); err != nil {
			return nil, err
		}
		r.Line = int(v)
		if v, err = getInt(); err != nil {
			return nil, err
		}
		r.TS = base + v
		if r.Sev, err = getStr(); err != nil {
			return nil, err
		}
		if r.Comp, err = getStr(); err != nil {
			return nil, err
		}
		if v, err = getInt(); err != nil {
			return nil, err
		}
		r.LogID = int(v)
		if r.Ctx, err = getStr(); err != nil {
			return nil, err
		}
		if r.Msg, err = getStr(); err != nil {
			return nil, err
		}
		if v, err = getInt(); err != nil {
			return nil, err
		}
		r.Truncated = v&1 != 0
		if r.attr, err = getStr(); err != nil {
			return nil, err
		}
		if r.Raw, err = getStr(); err != nil {
			return nil, err
		}
	}
	return records, nil
}

// blockCache keeps recently opened blocks, because showing a few lines around a moment opens the
// same block several times.
type blockCache struct {
	mu    sync.Mutex
	order []int64
	items map[int64][]Record
}

const cacheBlocks = 64

func (c *blockCache) get(id int64) ([]Record, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.items[id]
	return r, ok
}

func (c *blockCache) put(id int64, records []Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[int64][]Record{}
	}
	if _, ok := c.items[id]; ok {
		return
	}
	if len(c.order) >= cacheBlocks {
		delete(c.items, c.order[0])
		c.order = c.order[1:]
	}
	c.order = append(c.order, id)
	c.items[id] = records
}

func (s *Store) block(id int64) ([]Record, error) {
	if records, ok := s.cache.get(id); ok {
		return records, nil
	}
	var stream int64
	var data []byte
	if err := s.DB.QueryRow(`SELECT stream, data FROM block WHERE id = ?`, id).Scan(&stream, &data); err != nil {
		return nil, fmt.Errorf("block %d: %w", id, err)
	}
	records, err := DecodeBlock(id, stream, data)
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", id, err)
	}
	s.cache.put(id, records)
	return records, nil
}

// Line returns one stored line.
func (s *Store) Line(ref Ref) (*Record, error) {
	records, err := s.block(ref.Block)
	if err != nil {
		return nil, err
	}
	if ref.Slot < 0 || ref.Slot >= len(records) {
		return nil, fmt.Errorf("block %d has no line %d", ref.Block, ref.Slot)
	}
	r := records[ref.Slot]
	return &r, nil
}

// Query narrows a scan. Zero values mean no restriction.
type Query struct {
	Streams []int64
	From    int64
	To      int64
	SevMask int64
	IDs     []int
	Newest  bool
}

// Scan calls fn with every line the query admits, in time order (or newest first), merging the
// streams. fn returns false to stop.
func (s *Store) Scan(q Query, fn func(*Record) bool) error {
	var clauses []string
	var params []any
	if len(q.Streams) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(q.Streams)), ",")
		clauses = append(clauses, "stream IN ("+marks+")")
		for _, id := range q.Streams {
			params = append(params, id)
		}
	}
	if q.From != 0 {
		clauses = append(clauses, "last_ts >= ?")
		params = append(params, q.From)
	}
	if q.To != 0 {
		clauses = append(clauses, "first_ts <= ?")
		params = append(params, q.To)
	}
	if q.SevMask != 0 {
		clauses = append(clauses, "(sev_mask & ?) != 0")
		params = append(params, q.SevMask)
	}
	if len(q.IDs) > 0 {
		var ors []string
		for _, id := range q.IDs {
			ors = append(ors, "instr(ids, ?) > 0")
			params = append(params, fmt.Sprintf(",%d,", id))
		}
		clauses = append(clauses, "("+strings.Join(ors, " OR ")+")")
	}
	where := "1"
	if len(clauses) > 0 {
		where = strings.Join(clauses, " AND ")
	}
	order := "first_ts, id"
	if q.Newest {
		order = "last_ts DESC, id DESC"
	}
	rows, err := s.DB.Query(`SELECT id, stream, first_ts, last_ts FROM block WHERE `+where+` ORDER BY `+order, params...)
	if err != nil {
		return err
	}
	var metas []BlockMeta
	for rows.Next() {
		var m BlockMeta
		if err := rows.Scan(&m.ID, &m.Stream, &m.First, &m.Last); err != nil {
			rows.Close()
			return err
		}
		metas = append(metas, m)
	}
	rows.Close()

	wantID := map[int]bool{}
	for _, id := range q.IDs {
		wantID[id] = true
	}
	admit := func(r *Record) bool {
		if q.From != 0 && r.TS < q.From || q.To != 0 && r.TS > q.To {
			return false
		}
		if q.SevMask != 0 && SevBit(r.Sev)&q.SevMask == 0 {
			return false
		}
		if len(wantID) > 0 && !wantID[r.LogID] {
			return false
		}
		return true
	}

	// A k-way merge: blocks are opened in the order their first line comes due, and lines are
	// taken across the open blocks by time. Blocks of one stream overlap only at their edges, so
	// few are open at once.
	h := &cursorHeap{newest: q.Newest}
	next := 0
	due := func() int64 {
		if next >= len(metas) {
			return 0
		}
		if q.Newest {
			return metas[next].Last
		}
		return metas[next].First
	}
	ahead := s.prefetch(metas)
	defer ahead.stop()
	open := func() error {
		next++
		records, err := ahead.take(next - 1)
		if err != nil {
			return err
		}
		s.cache.put(metas[next-1].ID, records)
		c := &cursor{records: records, newest: q.Newest}
		if q.Newest {
			c.at = len(records) - 1
		}
		if c.valid() {
			heap.Push(h, c)
		}
		return nil
	}
	for next < len(metas) || h.Len() > 0 {
		// Open every block that could hold a line earlier (or later, newest first) than the
		// earliest line already open.
		for next < len(metas) && (h.Len() == 0 || h.before(due(), h.items[0].ts())) {
			if err := open(); err != nil {
				return err
			}
		}
		if h.Len() == 0 {
			continue
		}
		c := h.items[0]
		r := c.current()
		c.advance()
		if c.valid() {
			heap.Fix(h, 0)
		} else {
			heap.Pop(h)
		}
		if admit(r) && !fn(r) {
			return nil
		}
	}
	return nil
}

// prefetcher decodes the blocks a scan will open, in the order it will open them, a few at a time
// on other cores: one scan of a large bundle was one core inflating blocks while the rest idled.
type prefetcher struct {
	results []chan prefetched
	window  chan struct{}
	done    chan struct{}
	feeder  sync.WaitGroup
}

type prefetched struct {
	records []Record
	err     error
}

const (
	prefetchAhead   = 32 // blocks decoded or decoding ahead of the merge
	prefetchWorkers = 6
)

func (s *Store) prefetch(metas []BlockMeta) *prefetcher {
	p := &prefetcher{results: make([]chan prefetched, len(metas)), window: make(chan struct{}, prefetchAhead),
		done: make(chan struct{})}
	for i := range p.results {
		p.results[i] = make(chan prefetched, 1)
	}
	workers := make(chan struct{}, prefetchWorkers)
	p.feeder.Add(1)
	go func() {
		defer p.feeder.Done()
		for i, m := range metas {
			select {
			case p.window <- struct{}{}:
			case <-p.done:
				return
			}
			if records, ok := s.cache.get(m.ID); ok {
				p.results[i] <- prefetched{records: records}
				continue
			}
			// The store's one connection reads the blobs in turn; the decoding is what spreads.
			var stream int64
			var data []byte
			if err := s.DB.QueryRow(`SELECT stream, data FROM block WHERE id = ?`, m.ID).Scan(&stream, &data); err != nil {
				p.results[i] <- prefetched{err: fmt.Errorf("block %d: %w", m.ID, err)}
				continue
			}
			workers <- struct{}{}
			go func(slot int, id, stream int64, data []byte) {
				defer func() { <-workers }()
				records, err := DecodeBlock(id, stream, data)
				if err != nil {
					err = fmt.Errorf("block %d: %w", id, err)
				}
				p.results[slot] <- prefetched{records: records, err: err}
			}(i, m.ID, stream, data)
		}
	}()
	return p
}

// take waits for block i and frees its place in the window.
func (p *prefetcher) take(i int) ([]Record, error) {
	got := <-p.results[i]
	<-p.window
	return got.records, got.err
}

// stop ends the feeding when a scan stops early, and waits for it to let go of the connection.
func (p *prefetcher) stop() {
	close(p.done)
	p.feeder.Wait()
}

type cursor struct {
	records []Record
	at      int
	newest  bool
}

func (c *cursor) valid() bool      { return c.at >= 0 && c.at < len(c.records) }
func (c *cursor) current() *Record { return &c.records[c.at] }
func (c *cursor) ts() int64        { return c.records[c.at].TS }
func (c *cursor) advance() {
	if c.newest {
		c.at--
	} else {
		c.at++
	}
}

type cursorHeap struct {
	items  []*cursor
	newest bool
}

func (h *cursorHeap) before(a, b int64) bool {
	if h.newest {
		return a >= b
	}
	return a <= b
}
func (h cursorHeap) Len() int { return len(h.items) }
func (h cursorHeap) Less(i, j int) bool {
	a, b := h.items[i].ts(), h.items[j].ts()
	if a == b {
		return h.items[i].current().Block < h.items[j].current().Block
	}
	if h.newest {
		return a > b
	}
	return a < b
}
func (h cursorHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *cursorHeap) Push(x any)   { h.items = append(h.items, x.(*cursor)) }
func (h *cursorHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}
