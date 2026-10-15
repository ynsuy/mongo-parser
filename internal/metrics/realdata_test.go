package metrics

import (
	"math"
	"os"
	"testing"
	"time"

	"github.com/ynsuy/mongo-parser/internal/ftdc"
)

// wrapFloor is how far below zero a reading has to be before arithmetic is the only way it got
// there. A 32-bit column that accumulated past 2^31 and was narrowed back lands just under -2^31;
// anything a server reports about itself stays orders of magnitude above that.
//
// The two populations are not close. An int32 column narrowed back after passing 2^31 lands within
// a fraction of a percent of -2^31. The negatives a server legitimately reports — the replication
// buffer's size and count when a batch is accounted for twice, tcmalloc's -1 for an unset limit —
// are millions at most. Half of 2^31 sits in the gap with orders of magnitude either side of it.
const wrapFloor = -1 << 30

// A curated metric that goes a billion below zero has not been decoded. Bytes in a cache,
// milliseconds a disk was busy, pages written from it: none of them is negative, and none of them
// is negative by two billion for any reason a server could have.
//
// A small negative is left alone, because servers do report those. MongoDB's oplog apply buffer
// briefly goes below zero when a batch is accounted for twice, and tcmalloc reports -1 for a usage
// limit that is not set. Neither is this package's business to correct, and a test that flagged
// them would be describing the server rather than the decoder.
//
// It lives here rather than beside the decoder because the question is about the metrics anyone
// actually charts, and Curated is what lists them. What it catches is an int32 column narrowed
// back to the width its chunk declared: the type in a reference document says how the value was
// written, not how wide the thing being counted is, so a counter that still fitted in an int32 when
// the chunk opened goes on past 2^31 inside it. A cache byte counter is the kind of figure that
// does this, and it is the first number anyone reads about eviction pressure.
func TestNoCuratedMetricWrapsPastThirtyTwoBits(t *testing.T) {
	root := os.Getenv("DBA_FTDC_DIR")
	if root == "" {
		t.Skip("set DBA_FTDC_DIR to a directory of diagnostic data written by a real server")
	}
	stream, err := ftdc.OpenStream(root)
	if err != nil {
		t.Fatalf("opening %s: %v", root, err)
	}
	first, last := stream.Span()
	decoded, err := stream.Read(first, last, func(path string) bool { return Curated(path) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Values) == 0 {
		t.Fatal("no curated metric was decoded")
	}
	wrapped := 0
	for path, values := range decoded.Values {
		lowest, at := 0.0, -1
		for i, value := range values {
			if !math.IsNaN(value) && value < lowest {
				lowest, at = value, i
			}
		}
		if lowest < wrapFloor {
			wrapped++
			if wrapped <= 5 {
				t.Errorf("%s reaches %.0f at sample %d, which is 32-bit wraparound and not a reading",
					path, lowest, at)
			}
		}
	}
	t.Logf("%d curated metrics over %s", len(decoded.Values), last.Sub(first).Round(time.Minute))
}
