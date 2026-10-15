package ingest

import (
	"fmt"
	"strings"

	"github.com/ynsuy/mongo-parser/internal/store"
)

const (
	blockLines = 1024
	blockBytes = 1 << 20
)

// gap is a stretch of at least store.SilenceMinutes between two consecutive lines of one stream.
type gap struct {
	From, To      int64
	Before, After store.Ref
}

// builder packs one stream's lines into blocks as they are read, and notes along the way what
// would otherwise need a second pass: where its first and last lines are, and where it went quiet.
type builder struct {
	in     *ingester
	stream int64

	cur      []store.Record
	curID    int64
	curBytes int

	has               bool
	first, last       store.Ref
	maxTS             int64
	lastRef           store.Ref
	gaps              []gap
	truncated, blocks int
}

func (in *ingester) newBuilder(stream int64) *builder {
	return &builder{in: in, stream: stream}
}

func (b *builder) add(r store.Record) {
	if b.curID == 0 {
		b.curID = b.in.ids.block.Add(1)
	}
	r.Stream, r.Block, r.Slot = b.stream, b.curID, len(b.cur)
	b.track(&r)
	b.cur = append(b.cur, r)
	b.curBytes += len(r.Raw)
	if len(b.cur) >= blockLines || b.curBytes >= blockBytes {
		b.flush()
	}
}

// track notes where a line sits and whether a gap ends at it.
func (b *builder) track(r *store.Record) {
	ref := store.Ref{Block: r.Block, Slot: r.Slot}
	threshold := int64(store.SilenceMinutes * 60 * 1000)
	if !b.has {
		b.has, b.first, b.maxTS = true, ref, r.TS
	} else if r.TS-b.maxTS >= threshold {
		b.gaps = append(b.gaps, gap{From: b.maxTS, To: r.TS, Before: b.lastRef, After: ref})
	}
	// A line written out of order (a long operation logs its start time late) does not open a gap
	// that the lines around it close.
	if r.TS >= b.maxTS {
		b.maxTS = r.TS
		b.lastRef = ref
	}
	b.last = ref
	if r.Truncated {
		b.truncated++
	}
}

func (b *builder) flush() {
	if len(b.cur) == 0 {
		return
	}
	meta, ids := blockMeta(b.curID, b.stream, b.cur)
	b.in.blocks <- blockOut{meta: meta, ids: ids, data: store.EncodeBlock(b.cur)}
	b.blocks++
	b.cur, b.curID, b.curBytes = nil, 0, 0
}

// blockMeta is what the block table says about a block's lines.
func blockMeta(id, stream int64, records []store.Record) (store.BlockMeta, string) {
	meta := store.BlockMeta{ID: id, Stream: stream, Lines: len(records), First: records[0].TS, Last: records[0].TS}
	ids := map[int]bool{}
	for i := range records {
		r := &records[i]
		meta.First = min(meta.First, r.TS)
		meta.Last = max(meta.Last, r.TS)
		meta.SevMask |= store.SevBit(r.Sev)
		ids[r.LogID] = true
	}
	var list strings.Builder
	list.WriteString(",")
	for id := range ids {
		fmt.Fprintf(&list, "%d,", id)
	}
	return meta, list.String()
}

type blockOut struct {
	meta store.BlockMeta
	ids  string
	data []byte
}
