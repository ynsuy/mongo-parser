package ftdc

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"path/filepath"
	"sync"

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

type chunkData struct {
	timestamps []int64
	values     map[string][]float64
	kinds      map[string]string
}

// keepMasks remembers, for one read, which columns of each layout the selector keeps: asking the
// selector about every column of every chunk cost as much as the decode.
type keepMasks struct {
	sync.Mutex
	byLayout map[*layout][]bool
}

func (m *keepMasks) of(l *layout, want Selector) []bool {
	if want == nil {
		return nil
	}
	m.Lock()
	defer m.Unlock()
	if mask, ok := m.byLayout[l]; ok {
		return mask
	}
	mask := make([]bool, len(l.paths))
	for i, path := range l.paths {
		mask[i] = want(path)
	}
	if m.byLayout == nil {
		m.byLayout = map[*layout][]bool{}
	}
	m.byLayout[l] = mask
	return mask
}

// decodeChunk inflates one chunk and walks its delta stream.
// Unwanted columns still advance the cursor — the stream is column-major — but nothing is kept.
func decodeChunk(compressed []byte, want Selector, masks *keepMasks) (*chunkData, error) {
	payload, err := inflate(compressed)
	if err != nil {
		return nil, err
	}
	// Nothing decoded keeps a reference into the payload: values are copied out and paths are
	// built as strings of their own.
	defer payloads.Put(&payload)
	if len(payload) < 13 {
		return nil, fmt.Errorf("truncated FTDC chunk header")
	}
	referenceLength := int(binary.LittleEndian.Uint32(payload))
	if referenceLength < 5 || referenceLength > len(payload)-8 {
		return nil, fmt.Errorf("invalid FTDC reference document length")
	}
	metricCount := int(binary.LittleEndian.Uint32(payload[referenceLength:]))
	deltaCount := int(binary.LittleEndian.Uint32(payload[referenceLength+4:]))

	shape, starts, err := layoutOf(payload[:referenceLength])
	if err != nil {
		return nil, err
	}
	if len(shape.paths) != metricCount {
		return nil, fmt.Errorf("FTDC attribute count mismatch: reference has %d, header claims %d", len(shape.paths), metricCount)
	}
	if deltaCount < 0 || metricCount*(deltaCount+1) > 64<<20 {
		return nil, fmt.Errorf("FTDC samples exceed the decoder budget")
	}
	keep := masks.of(shape, want)

	kept := metricCount
	if keep != nil {
		kept = 0
		for _, k := range keep {
			if k {
				kept++
			}
		}
	}
	chunk := &chunkData{values: make(map[string][]float64, kept), kinds: make(map[string]string, kept)}
	deltas := payload[referenceLength+8:]
	var zerosLeft uint64
	for column, path := range shape.paths {
		keeping := keep == nil || keep[column]
		isClock := column == shape.clock
		var values []float64
		var raw []uint64
		if keeping {
			values = make([]float64, 0, deltaCount+1)
			values = append(values, float64(int64(starts[column])))
		}
		if isClock {
			raw = make([]uint64, 0, deltaCount+1)
			raw = append(raw, starts[column])
		}
		accumulated := starts[column]
		for range deltaCount {
			var delta uint64
			if zerosLeft > 0 {
				zerosLeft--
			} else {
				var n int
				delta, n = binary.Uvarint(deltas)
				if n <= 0 {
					return nil, fmt.Errorf("truncated FTDC deltas at %s", path)
				}
				deltas = deltas[n:]
				if delta == 0 {
					// A literal zero is followed by the count of further zeros, and that run
					// continues across column boundaries.
					zerosLeft, n = binary.Uvarint(deltas)
					if n <= 0 {
						return nil, fmt.Errorf("invalid FTDC zero run at %s", path)
					}
					deltas = deltas[n:]
				}
			}
			accumulated += delta
			if keeping {
				values = append(values, float64(int64(accumulated)))
			}
			if isClock {
				raw = append(raw, accumulated)
			}
		}
		if keeping {
			chunk.values[path] = values
			chunk.kinds[path] = shape.kinds[column]
		}
		if isClock {
			chunk.timestamps = make([]int64, len(raw))
			for i, value := range raw {
				chunk.timestamps[i] = int64(value)
			}
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

var zlibReaders sync.Pool

// payloads are inflated chunks' buffers, handed back once a chunk is decoded.
var payloads sync.Pool

func inflate(compressed []byte) ([]byte, error) {
	if len(compressed) < 4 {
		return nil, fmt.Errorf("truncated FTDC chunk")
	}
	declared := int(binary.LittleEndian.Uint32(compressed))
	if declared < 0 || declared > maxInflated {
		return nil, fmt.Errorf("FTDC chunk exceeds the decoder budget")
	}
	source := bytes.NewReader(compressed[4:])
	var reader io.ReadCloser
	if pooled, ok := zlibReaders.Get().(io.ReadCloser); ok && pooled.(zlib.Resetter).Reset(source, nil) == nil {
		reader = pooled
	} else {
		var err error
		if reader, err = zlib.NewReader(source); err != nil {
			return nil, err
		}
	}
	defer func() {
		reader.Close()
		zlibReaders.Put(reader)
	}()
	// The declared length sizes the buffer once; a chunk that inflates to more still reads whole,
	// up to the budget.
	backing := make([]byte, 0, declared+bytes.MinRead)
	if pooled, ok := payloads.Get().(*[]byte); ok && cap(*pooled) >= declared+bytes.MinRead {
		backing = (*pooled)[:0]
	}
	buffer := bytes.NewBuffer(backing)
	if _, err := buffer.ReadFrom(io.LimitReader(reader, maxInflated+1)); err != nil {
		return nil, err
	}
	if buffer.Len() > maxInflated {
		return nil, fmt.Errorf("FTDC chunk exceeds the decoder budget")
	}
	return buffer.Bytes(), nil
}
