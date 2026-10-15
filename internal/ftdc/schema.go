// Package ftdc decodes MongoDB full-time diagnostic capture files.
//
// A file is a sequence of BSON documents. Type 0 carries metadata; type 1 carries a chunk whose
// binary payload is a 4-byte uncompressed length followed by a zlib stream. An inflated chunk is a
// reference BSON document, then metricCount and deltaCount as little-endian uint32, then
// column-major varint deltas. Column order comes from a depth-first walk of the reference document.
package ftdc

import (
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
)

// column is one metric of a chunk: where it sits in the delta stream and how to read its values.
type column struct {
	path  string
	kind  bsontype.Type
	start uint64
}

// Metric paths join with "/" and a Timestamp becomes two columns, matching the convention the
// rest of this tool already uses.
//
// A double is carried as its value truncated to an integer, which is what the server does:
// FTDCBSONUtil::extractMetricsFromDocument casts a NumberDouble to uint64 before it ever reaches
// the delta stream, so the fraction is gone before the file is written and the deltas are
// differences of whole numbers.
//
// github.com/mongodb/ftdc, the Go port this package's round-trip test used to encode with, instead
// keeps the IEEE-754 bit pattern (normalizeFloat is int64(math.Float64bits(in))). A decoder can
// match one or the other and not both. Reading a server's file the port's way leaves every chunk's
// first sample right and every sample after it wrong, because the deltas land in the exponent and
// mantissa rather than in the value: systemMetrics/cpu/idle_ms then advances by a few milliseconds a
// second instead of roughly a thousand per idle core, and makes up the whole difference in one jump
// at each chunk boundary. Read as truncated integers it advances steadily, with no jumps at all.
//
// A timestamp's seconds are kept raw. mongodb/ftdc scales its reference value by 1000 but emits
// unscaled per-second deltas, so a decoder that scales drifts by a factor of 1000 after the first
// sample; TestRoundTripThroughReferenceEncoder pins this down.
func columns(reference bson.Raw) ([]column, error) {
	found := make([]column, 0, 512)
	if err := walk(reference, "", &found); err != nil {
		return nil, err
	}
	unwrapServices(found)
	return found, nil
}

// serviceSections are the top-level documents 8.0 nests a sample under when a process runs more
// than one service: every mongod started with --shardsvr or --configsvr, and mongos
// (featureFlagMultiServiceLogAndFTDCFormat, on by default; FTDCCollectorCollection::collect in
// src/mongo/db/ftdc/collector.cpp at r8.0.0). serverStatus and systemMetrics move under common/
// and replSetGetStatus under shard/. router/ is left alone: it is the second service's own view and
// would collide with the first's.
var serviceSections = []string{"common/", "shard/"}

// unwrapServices gives 8.0's nested paths the names every earlier version and every replica set
// uses, so a metric is one path whatever the topology. A path that would land on a name another
// column already has keeps its prefix: two counters are never merged into one.
func unwrapServices(found []column) {
	taken := make(map[string]bool, len(found))
	for _, c := range found {
		taken[c.path] = true
	}
	for i, c := range found {
		for _, section := range serviceSections {
			bare, nested := strings.CutPrefix(c.path, section)
			if !nested || taken[bare] {
				continue
			}
			taken[bare] = true
			found[i].path = bare
			break
		}
	}
}

func walk(document bson.Raw, prefix string, into *[]column) error {
	elements, err := document.Elements()
	if err != nil {
		return err
	}
	for _, element := range elements {
		key, err := element.KeyErr()
		if err != nil {
			return err
		}
		path := key
		if prefix != "" {
			path = prefix + "/" + key
		}
		if err := walkValue(element.Value(), path, into); err != nil {
			return err
		}
	}
	return nil
}

func walkValue(value bson.RawValue, path string, into *[]column) error {
	switch value.Type {
	case bsontype.EmbeddedDocument:
		nested, ok := value.DocumentOK()
		if !ok {
			return fmt.Errorf("malformed embedded document at %s", path)
		}
		return walk(nested, path, into)
	case bsontype.Array:
		array, ok := value.ArrayOK()
		if !ok {
			return fmt.Errorf("malformed array at %s", path)
		}
		return walk(bson.Raw(array), path, into)
	case bsontype.Double:
		number, ok := value.DoubleOK()
		if !ok {
			return fmt.Errorf("malformed double at %s", path)
		}
		*into = append(*into, column{path, value.Type, uint64(int64(number))})
	case bsontype.Int32:
		number, ok := value.Int32OK()
		if !ok {
			return fmt.Errorf("malformed int32 at %s", path)
		}
		*into = append(*into, column{path, value.Type, uint64(int64(number))})
	case bsontype.Int64:
		number, ok := value.Int64OK()
		if !ok {
			return fmt.Errorf("malformed int64 at %s", path)
		}
		*into = append(*into, column{path, value.Type, uint64(number)})
	case bsontype.Boolean:
		flag, ok := value.BooleanOK()
		if !ok {
			return fmt.Errorf("malformed bool at %s", path)
		}
		var number uint64
		if flag {
			number = 1
		}
		*into = append(*into, column{path, value.Type, number})
	case bsontype.DateTime:
		millis, ok := value.DateTimeOK()
		if !ok {
			return fmt.Errorf("malformed datetime at %s", path)
		}
		*into = append(*into, column{path, value.Type, uint64(millis)})
	case bsontype.Timestamp:
		seconds, increment, ok := value.TimestampOK()
		if !ok {
			return fmt.Errorf("malformed timestamp at %s", path)
		}
		*into = append(*into,
			column{path + "/t", bsontype.Int64, uint64(seconds)},
			column{path + "/i", bsontype.Int64, uint64(increment)})
	}
	// Everything else — string, ObjectID, Decimal128, null, binary, regex — is not a metric.
	return nil
}

// value is the accumulated column read back as a number. There is nothing type-specific left to
// undo: every column in the metric stream accumulates as an integer, whatever BSON type the
// reference document declared it with.
//
// Both halves of that sentence were learned the hard way, and the declared type is kept only so
// that Kinds can report it.
//
// A double is already truncated when it reaches the stream — the server casts a NumberDouble to
// uint64 before encoding it — so there is no bit pattern to reconstruct, and treating the deltas as
// IEEE-754 bits left every sample inside a chunk wrong while every chunk's first sample stayed
// right. An int32 is not narrowed back either: the declared type says how the value was written at
// the start of a chunk, not how wide the thing being counted is, so a counter that still fitted in
// an int32 goes on accumulating past 2^31 inside the chunk. Narrowed there it turns negative and
// stays so until the next chunk's reference value rescues it, leaving any large counter below zero
// for the rest of that chunk.
//
// The columns the narrowing was written for are WiredTiger's packed timestamps, and they are no
// worse without it: a timestamp is a second in the high half and a counter in the low half, so its
// steps really are 2^32-sized, and carrying them whole removes their negatives too.
func (c column) value(accumulated uint64) float64 {
	return float64(int64(accumulated))
}
