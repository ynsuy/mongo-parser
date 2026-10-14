package ingest

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	"mongo-log-parser/internal/store"
)

// Copies are not always whole files. A node's current log is often also the head of the archive
// that rotated it, or one export overlaps the next by an hour; the files differ, so neither is an
// exact copy of the other, and every line in the overlap was counted twice. Lines are compared
// where two streams overlap in time, and a line the other stream already holds is dropped.

// lineKey identifies a line by what the server wrote. A line read out of a journal carries the
// journal's prefix in front of the same JSON, and is the same line.
func lineKey(r *store.Record) uint64 {
	text := r.Raw
	if i := strings.Index(text, `{"t":`); i > 0 {
		text = text[i:]
	}
	h := fnv.New64a()
	h.Write([]byte(text))
	return h.Sum64()
}

// storedBlocks reads a stream's blocks that hold lines in [from, to], in the order they were written.
func (in *ingester) storedBlocks(stream, from, to int64) ([][]store.Record, error) {
	return in.blocksAround(stream, from, to, 0)
}

// blocksAround reads up to limit (0: all) of a stream's blocks that hold lines in [from, to]. They
// are walked by where they end, and the walk stops once blocks begin past to: a stream's blocks are
// written in time order, so asking by start time alone read every block of the stream before it.
func (in *ingester) blocksAround(stream, from, to int64, limit int) ([][]store.Record, error) {
	rows, err := in.st.DB.Query(`SELECT id, first_ts, data FROM block WHERE stream = ? AND last_ts >= ? ORDER BY last_ts, id`, stream, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]store.Record
	past := 0
	for rows.Next() {
		var id, first int64
		var data []byte
		if err := rows.Scan(&id, &first, &data); err != nil {
			return nil, err
		}
		if first > to {
			// A block can start late and still end early when a line was written out of order, so
			// a few are looked past before the walk gives up.
			if past++; past > 8 {
				break
			}
			continue
		}
		records, err := store.DecodeBlock(id, stream, data)
		if err != nil {
			return nil, err
		}
		out = append(out, records)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0].Block < out[j][0].Block })
	return out, rows.Err()
}

// rewrite replaces the stream's blocks that hold lines in [from, to] with what change makes of
// each, and drops a block left empty.
func (in *ingester) rewrite(stream, from, to int64, change func([]store.Record) []store.Record) error {
	blocks, err := in.storedBlocks(stream, from, to)
	if err != nil {
		return err
	}
	for _, records := range blocks {
		id := records[0].Block
		kept := change(records)
		if len(kept) == 0 {
			if _, err := in.st.DB.Exec(`DELETE FROM block WHERE id = ?`, id); err != nil {
				return err
			}
			continue
		}
		meta, ids := blockMeta(id, stream, kept)
		if _, err := in.st.DB.Exec(`UPDATE block SET first_ts = ?, last_ts = ?, lines = ?, sev_mask = ?, ids = ?, data = ? WHERE id = ?`,
			meta.First, meta.Last, meta.Lines, meta.SevMask, ids, store.EncodeBlock(kept), id); err != nil {
			return err
		}
	}
	return nil
}

// rescan recounts a stream from its stored blocks once they have been rewritten: its span, its
// line count, and where its first and last lines and its gaps now are.
func (in *ingester) rescan(s *streamRow) error {
	blocks, err := in.storedBlocks(s.ID, minTS, maxTS)
	if err != nil {
		return err
	}
	b := in.newBuilder(s.ID)
	lines, first, last := 0, int64(0), int64(0)
	for _, records := range blocks {
		for i := range records {
			r := &records[i]
			b.track(r)
			if lines == 0 || r.TS < first {
				first = r.TS
			}
			last = max(last, r.TS)
			lines++
		}
	}
	s.Stats.Lines, s.Stats.First, s.Stats.Last = lines, first, last
	b.blocks = len(blocks)
	s.built = b
	return nil
}

const (
	minTS = -1 << 62
	maxTS = 1 << 62
)

// dropOverlaps removes from each stream the lines a larger stream overlapping it in time also holds.
func (in *ingester) dropOverlaps() error {
	var streams []*streamRow
	for i := range in.streams {
		if s := &in.streams[i]; s.Stats.Lines > 0 && s.built != nil && s.built.has {
			streams = append(streams, s)
		}
	}
	sort.SliceStable(streams, func(i, j int) bool {
		if streams[i].Stats.Lines != streams[j].Stats.Lines {
			return streams[i].Stats.Lines > streams[j].Stats.Lines
		}
		return streams[i].ID < streams[j].ID
	})
	changed := map[*streamRow]int{}
	for i, big := range streams {
		for _, small := range streams[i+1:] {
			from, to := max(big.Stats.First, small.Stats.First), min(big.Stats.Last, small.Stats.Last)
			if from > to || differentWriters(big, small) {
				continue
			}
			copied, err := in.looksCopied(big, small, from, to)
			if err != nil {
				return err
			}
			if !copied {
				continue
			}
			held := map[uint64]int{}
			blocks, err := in.storedBlocks(big.ID, from, to)
			if err != nil {
				return err
			}
			for _, records := range blocks {
				for k := range records {
					if r := &records[k]; r.TS >= from && r.TS <= to {
						held[lineKey(r)]++
					}
				}
			}
			removed := 0
			err = in.rewrite(small.ID, from, to, func(records []store.Record) []store.Record {
				kept := records[:0]
				for _, r := range records {
					if r.TS >= from && r.TS <= to {
						if key := lineKey(&r); held[key] > 0 {
							held[key]--
							removed++
							continue
						}
					}
					kept = append(kept, r)
				}
				return kept
			})
			if err != nil {
				return err
			}
			if removed > 0 {
				changed[small] += removed
				in.overlapNote(small, big, removed)
			}
		}
	}
	for s := range changed {
		if err := in.rescan(s); err != nil {
			return err
		}
		// A stream left with nothing was a copy after all, and goes the way an exact copy does.
		if s.Stats.Lines == 0 {
			in.dupStreams[s.ID] = true
		}
	}
	kept := make([]streamRow, 0, len(in.streams))
	for _, s := range in.streams {
		if !in.dupStreams[s.ID] {
			kept = append(kept, s)
		}
	}
	in.streams = kept
	return nil
}

// differentWriters reports whether two streams say they were written by different processes.
func differentWriters(a, b *streamRow) bool {
	name := func(s *streamRow) string {
		id := s.Stats.Identity
		switch {
		case id.Pod != "":
			return "pod " + id.Pod + "@" + id.Site
		case id.Self != "":
			return "self " + strings.ToLower(id.Self)
		case id.Host != "" && id.Port != 0:
			return fmt.Sprintf("host %s:%d", strings.ToLower(id.Host), id.Port)
		}
		return ""
	}
	x, y := name(a), name(b)
	return x != "" && y != "" && x != y
}

// looksCopied samples the first block of small inside the overlap: most of its lines are in big
// when one is a copy of the other, and almost none when two processes merely logged at the same
// time. Comparing everything to find that out would read both logs whole.
func (in *ingester) looksCopied(big, small *streamRow, from, to int64) (bool, error) {
	blocks, err := in.blocksAround(small.ID, from, to, 1)
	if err != nil || len(blocks) == 0 {
		return false, err
	}
	sample := map[uint64]bool{}
	lo, hi := int64(maxTS), int64(minTS)
	for _, records := range blocks[:1] {
		for k := range records {
			if r := &records[k]; r.TS >= from && r.TS <= to {
				sample[lineKey(r)] = true
				lo, hi = min(lo, r.TS), max(hi, r.TS)
			}
		}
	}
	if len(sample) == 0 {
		return false, nil
	}
	theirs, err := in.storedBlocks(big.ID, lo, hi)
	if err != nil {
		return false, err
	}
	found := 0
	for _, records := range theirs {
		for k := range records {
			if sample[lineKey(&records[k])] {
				found++
			}
		}
	}
	return found*2 >= len(sample), nil
}

// overlapNote says on a source how many of its lines another file already held.
func (in *ingester) overlapNote(small, big *streamRow, removed int) {
	bigPath := ""
	for _, src := range in.sources {
		if src.ID == big.Source {
			bigPath = src.Path
		}
	}
	in.setNote(small.Source, fmt.Sprintf("%d lines that %s also holds were read once", removed, bigPath))
}

// applyShifts moves the times of the streams that were written in local time to UTC.
func (in *ingester) applyShifts() error {
	for i := range in.streams {
		s := &in.streams[i]
		offset := in.shifts[s.ID]
		if offset == 0 {
			continue
		}
		err := in.rewrite(s.ID, minTS, maxTS, func(records []store.Record) []store.Record {
			for k := range records {
				records[k].TS -= offset
			}
			return records
		})
		if err != nil {
			return err
		}
		if err := in.rescan(s); err != nil {
			return err
		}
	}
	return nil
}
