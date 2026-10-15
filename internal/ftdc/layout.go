package ftdc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
)

// A capture's reference documents nearly all share one schema: the values change from chunk to
// chunk, the keys and their order do not. Building every column's path from scratch for every
// chunk was most of what decoding allocated, so a chunk's schema is fingerprinted with a scan that
// builds nothing, and the paths are built once per schema.

// layout is the columns of one schema, in delta-stream order.
type layout struct {
	paths []string
	kinds []string
	clock int // the column the sample clock is read from, or -1
}

var layouts = struct {
	sync.Mutex
	byShape map[uint64]*layout
}{byShape: map[uint64]*layout{}}

var errShape = errors.New("reference document does not scan")

// layoutOf returns the reference document's layout and every column's starting value.
func layoutOf(reference []byte) (*layout, []uint64, error) {
	starts := make([]uint64, 0, 1024)
	hash, starts, err := scanShape(reference, fnvOffset, starts)
	if err == nil {
		layouts.Lock()
		cached := layouts.byShape[hash]
		layouts.Unlock()
		if cached != nil && len(cached.paths) == len(starts) {
			return cached, starts, nil
		}
	}
	// A schema not seen yet, or one the scan cannot read: the full walk decides, and reports
	// whatever is malformed exactly as it always has.
	found, walkErr := columns(bson.Raw(reference))
	if walkErr != nil {
		return nil, nil, walkErr
	}
	built := &layout{paths: make([]string, len(found)), kinds: make([]string, len(found)), clock: -1}
	starts = starts[:0]
	for i, c := range found {
		built.paths[i] = c.path
		built.kinds[i] = c.kind.String()
		starts = append(starts, c.start)
	}
	for _, path := range clockPaths {
		for i, c := range found {
			if built.clock < 0 && c.path == path {
				built.clock = i
			}
		}
	}
	if err == nil {
		layouts.Lock()
		if len(layouts.byShape) > 256 {
			clear(layouts.byShape)
		}
		layouts.byShape[hash] = built
		layouts.Unlock()
	}
	return built, starts, nil
}

const (
	fnvOffset = 14695981039346656037
	fnvPrime  = 1099511628211
)

func mix(hash uint64, b byte) uint64 { return (hash ^ uint64(b)) * fnvPrime }

// scanShape hashes a document's keys, types and nesting, and appends each metric's starting value
// the way walkValue reads it.
func scanShape(document []byte, hash uint64, starts []uint64) (uint64, []uint64, error) {
	if len(document) < 5 {
		return 0, nil, errShape
	}
	size := int(int32(binary.LittleEndian.Uint32(document)))
	if size < 5 || size > len(document) || document[size-1] != 0 {
		return 0, nil, errShape
	}
	body := document[4 : size-1]
	hash = mix(hash, '{')
	for i := 0; i < len(body); {
		kind := body[i]
		i++
		end := bytes.IndexByte(body[i:], 0)
		if end < 0 {
			return 0, nil, errShape
		}
		hash = mix(hash, kind)
		for _, b := range body[i : i+end] {
			hash = mix(hash, b)
		}
		hash = mix(hash, 0)
		i += end + 1
		value := body[i:]
		need := func(n int) bool { return n >= 0 && len(value) >= n }
		switch kind {
		case 0x01: // double
			if !need(8) {
				return 0, nil, errShape
			}
			starts = append(starts, uint64(int64(math.Float64frombits(binary.LittleEndian.Uint64(value)))))
			i += 8
		case 0x10: // int32
			if !need(4) {
				return 0, nil, errShape
			}
			starts = append(starts, uint64(int64(int32(binary.LittleEndian.Uint32(value)))))
			i += 4
		case 0x12, 0x09: // int64, datetime
			if !need(8) {
				return 0, nil, errShape
			}
			starts = append(starts, binary.LittleEndian.Uint64(value))
			i += 8
		case 0x08: // boolean
			if !need(1) {
				return 0, nil, errShape
			}
			var flag uint64
			if value[0] == 1 {
				flag = 1
			}
			starts = append(starts, flag)
			i++
		case 0x11: // timestamp: increment, then seconds; walked as /t then /i
			if !need(8) {
				return 0, nil, errShape
			}
			starts = append(starts, uint64(binary.LittleEndian.Uint32(value[4:])), uint64(binary.LittleEndian.Uint32(value)))
			i += 8
		case 0x03, 0x04: // document, array
			if !need(4) {
				return 0, nil, errShape
			}
			n := int(int32(binary.LittleEndian.Uint32(value)))
			if !need(n) {
				return 0, nil, errShape
			}
			var err error
			if hash, starts, err = scanShape(value[:n], hash, starts); err != nil {
				return 0, nil, err
			}
			i += n
		case 0x02, 0x0D, 0x0E: // string, javascript, symbol
			if !need(4) {
				return 0, nil, errShape
			}
			n := 4 + int(int32(binary.LittleEndian.Uint32(value)))
			if !need(n) {
				return 0, nil, errShape
			}
			i += n
		case 0x05: // binary
			if !need(5) {
				return 0, nil, errShape
			}
			n := 5 + int(int32(binary.LittleEndian.Uint32(value)))
			if !need(n) {
				return 0, nil, errShape
			}
			i += n
		case 0x0F: // code with scope: its length includes itself
			if !need(4) {
				return 0, nil, errShape
			}
			n := int(int32(binary.LittleEndian.Uint32(value)))
			if !need(n) {
				return 0, nil, errShape
			}
			i += n
		case 0x07: // ObjectId
			if !need(12) {
				return 0, nil, errShape
			}
			i += 12
		case 0x13: // decimal128
			if !need(16) {
				return 0, nil, errShape
			}
			i += 16
		case 0x06, 0x0A, 0x7F, 0xFF: // undefined, null, maxkey, minkey
		default: // regex, DBPointer and anything unknown: leave it to the full walk
			return 0, nil, errShape
		}
	}
	return mix(hash, '}'), starts, nil
}
