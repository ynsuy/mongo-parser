package ftdc

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"path/filepath"

	"go.mongodb.org/mongo-driver/bson"
)

// maxInflated bounds one chunk so a corrupt length cannot exhaust memory.
const maxInflated = 256 << 20

// Selector decides which metric paths are materialized. A nil selector keeps every path.
type Selector func(path string) bool

// Samples holds decoded values aligned to one timestamp axis. A NaN means the metric was absent
// from that chunk: FTDC schemas drift between chunks and a missing column is a gap, not a repeat.
type Samples struct {
	Timestamps []int64 // epoch milliseconds, ascending
	Values     map[string][]float64
	Kinds      map[string]string // metric path -> BSON type it was stored as
	Skipped    int               // chunks that could not be decoded and are missing from the series
	Damaged    []string          // the first few of them, as "file: what went wrong"
}

// damaged records a chunk that could not be decoded. A capture cut off mid-write, or a byte that
// did not survive being copied out of a container, is a hole in one node's series — not a reason
// to answer nothing about any node. What was lost is counted and named here, because a gap nobody
// mentions reads as a server that went quiet.
func (s *Samples) damaged(path string, err error) {
	s.Skipped++
	note := filepath.Base(path) + ": " + err.Error()
	for _, seen := range s.Damaged {
		if seen == note {
			return
		}
	}
	if len(s.Damaged) < 3 {
		s.Damaged = append(s.Damaged, note)
	}
}

// clockPaths are the columns that carry a chunk's sample clock, most specific first.
var clockPaths = []string{"serverStatus/start", "start"}

func (s *Samples) append(chunk *chunkData) {
	base := len(s.Timestamps)
	for path, kind := range chunk.kinds {
		s.Kinds[path] = kind
	}
	s.Timestamps = append(s.Timestamps, chunk.timestamps...)
	for path, values := range chunk.values {
		series, seen := s.Values[path]
		if !seen {
			series = make([]float64, 0, base+len(values))
			for range base {
				series = append(series, math.NaN())
			}
		}
		for len(series) < base {
			series = append(series, math.NaN())
		}
		s.Values[path] = append(series, values...)
	}
	// Paths this chunk did not carry stay aligned by padding with gaps.
	for path, series := range s.Values {
		if _, present := chunk.values[path]; present {
			continue
		}
		for len(series) < len(s.Timestamps) {
			series = append(series, math.NaN())
		}
		s.Values[path] = series
	}
}

type chunkData struct {
	timestamps []int64
	values     map[string][]float64
	kinds      map[string]string
}

// decodeChunk inflates one chunk and walks its delta stream.
// Unwanted columns still advance the cursor — the stream is column-major — but nothing is kept.
func decodeChunk(compressed []byte, want Selector) (*chunkData, error) {
	payload, err := inflate(compressed)
	if err != nil {
		return nil, err
	}
	if len(payload) < 13 {
		return nil, fmt.Errorf("truncated FTDC chunk header")
	}
	referenceLength := int(binary.LittleEndian.Uint32(payload))
	if referenceLength < 5 || referenceLength > len(payload)-8 {
		return nil, fmt.Errorf("invalid FTDC reference document length")
	}
	reference := bson.Raw(payload[:referenceLength])
	metricCount := int(binary.LittleEndian.Uint32(payload[referenceLength:]))
	deltaCount := int(binary.LittleEndian.Uint32(payload[referenceLength+4:]))

	found, err := columns(reference)
	if err != nil {
		return nil, err
	}
	if len(found) != metricCount {
		return nil, fmt.Errorf("FTDC attribute count mismatch: reference has %d, header claims %d", len(found), metricCount)
	}
	if deltaCount < 0 || metricCount*(deltaCount+1) > 64<<20 {
		return nil, fmt.Errorf("FTDC samples exceed the decoder budget")
	}

	chunk := &chunkData{values: make(map[string][]float64, metricCount), kinds: make(map[string]string, metricCount)}
	clocks := map[string][]uint64{}
	reader := bytes.NewReader(payload[referenceLength+8:])
	var zerosLeft uint64
	for _, c := range found {
		keep := want == nil || want(c.path)
		isClock := false
		for _, path := range clockPaths {
			isClock = isClock || c.path == path
		}
		var values []float64
		var raw []uint64
		if keep {
			values = make([]float64, 0, deltaCount+1)
			values = append(values, c.value(c.start))
		}
		if isClock {
			raw = make([]uint64, 0, deltaCount+1)
			raw = append(raw, c.start)
		}
		accumulated := c.start
		for range deltaCount {
			var delta uint64
			if zerosLeft > 0 {
				zerosLeft--
			} else {
				delta, err = binary.ReadUvarint(reader)
				if err != nil {
					return nil, fmt.Errorf("truncated FTDC deltas at %s: %w", c.path, err)
				}
				if delta == 0 {
					// A literal zero is followed by the count of further zeros, and that run
					// continues across column boundaries.
					zerosLeft, err = binary.ReadUvarint(reader)
					if err != nil {
						return nil, fmt.Errorf("invalid FTDC zero run at %s: %w", c.path, err)
					}
				}
			}
			accumulated += delta
			if keep {
				values = append(values, c.value(accumulated))
			}
			if isClock {
				raw = append(raw, accumulated)
			}
		}
		if keep {
			chunk.values[c.path] = values
			chunk.kinds[c.path] = c.kind.String()
		}
		if isClock {
			clocks[c.path] = raw
		}
	}

	for _, path := range clockPaths {
		if raw, ok := clocks[path]; ok {
			chunk.timestamps = make([]int64, len(raw))
			for i, value := range raw {
				chunk.timestamps[i] = int64(value)
			}
			break
		}
	}
	if len(chunk.timestamps) == 0 {
		return nil, fmt.Errorf("FTDC chunk has no sample clock")
	}
	return chunk, nil
}

// referenceOf inflates only as much of a chunk as the reference document needs, which is how a
// chunk's start time is learned without paying for its deltas.
func referenceOf(compressed []byte) (bson.Raw, error) {
	if len(compressed) < 4 {
		return nil, fmt.Errorf("truncated FTDC chunk")
	}
	reader, err := zlib.NewReader(bytes.NewReader(compressed[4:]))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(reader, head); err != nil {
		return nil, err
	}
	length := int(binary.LittleEndian.Uint32(head))
	if length < 5 || length > maxInflated {
		return nil, fmt.Errorf("invalid FTDC reference document length")
	}
	document := make([]byte, length)
	copy(document, head)
	if _, err := io.ReadFull(reader, document[4:]); err != nil {
		return nil, err
	}
	return bson.Raw(document), nil
}

func inflate(compressed []byte) ([]byte, error) {
	if len(compressed) < 4 {
		return nil, fmt.Errorf("truncated FTDC chunk")
	}
	declared := int(binary.LittleEndian.Uint32(compressed))
	if declared < 0 || declared > maxInflated {
		return nil, fmt.Errorf("FTDC chunk exceeds the decoder budget")
	}
	reader, err := zlib.NewReader(bytes.NewReader(compressed[4:]))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	payload, err := io.ReadAll(io.LimitReader(reader, maxInflated+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxInflated {
		return nil, fmt.Errorf("FTDC chunk exceeds the decoder budget")
	}
	return payload, nil
}
