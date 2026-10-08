package ftdc

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/evergreen-ci/birch"
	reference "github.com/mongodb/ftdc"
	"go.mongodb.org/mongo-driver/bson/bsontype"
)

// TestRealFileAgainstReference decodes diagnostic data written by a real server with both this
// package and MongoDB's own, and compares every metric sample by sample.
//
// The round-trip test beside it proves the decoder against data this process encoded a moment
// earlier, which is worth something and is not the same thing. A file off a production node has
// hundreds of chunks, schemas that drift between them, metrics that appear and vanish, names
// carrying spaces and slashes, and whatever the version running there happened to write.
//
// Point DBA_FTDC_FILE at a file or at a directory of them. It is opt-in because the files it wants
// are customer data and do not belong in a repository.
func TestRealFileAgainstReference(t *testing.T) {
	root := os.Getenv("DBA_FTDC_FILE")
	if root == "" {
		t.Skip("set DBA_FTDC_FILE to a diagnostic data file or directory to compare against the reference decoder")
	}
	for _, path := range ftdcFiles(t, root) {
		t.Run(filepathBase(path), func(t *testing.T) { compareWithReference(t, path) })
	}
}

func filepathBase(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// ftdcFiles is the one file named, or every diagnostic data file under the directory named.
func ftdcFiles(t *testing.T, root string) []string {
	t.Helper()
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		return []string{root}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	found := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "metrics.") {
			found = append(found, root+"/"+entry.Name())
		}
	}
	sort.Strings(found)
	if len(found) == 0 {
		t.Fatalf("no diagnostic data files under %s", root)
	}
	return found
}

// exactFloat is the largest integer a float64 holds without rounding. This package keeps every
// metric as a float64, so anything above it is approximate — a handful of metrics, WiredTiger's
// internal hybrid clock stamps, whose magnitude means nothing to a reader. Compared as floats they would fail on the rounding alone,
// which says nothing about the decoding, so they are counted and skipped.
const exactFloat = 1 << 53

func compareWithReference(t *testing.T, path string) {
	file, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mine, err := file.Read(time.Time{}, time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	handle, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	theirs := map[string][]int64{}
	damaged := false
	iterator := reference.ReadSeries(context.Background(), handle)
	for iterator.Next() {
		for _, element := range iterator.Document().Elements() {
			array := element.Value().MutableArray()
			if array == nil {
				continue
			}
			values := make([]int64, 0, array.Len())
			for i := 0; i < array.Len(); i++ {
				n, ok := numberOf(array.Lookup(uint(i)))
				if !ok {
					continue
				}
				values = append(values, n)
			}
			theirs[element.Key()] = append(theirs[element.Key()], values...)
		}
	}
	if err := iterator.Err(); err != nil {
		// A capture cut off mid-write ends inside a document. This package indexes every whole
		// chunk before the damage and records where it stopped (File.Cut, which coverage prints);
		// the reference decodes the same whole chunks and then reports the trailer as an error.
		// They agree on everything there is to compare, so the comparison goes ahead on that,
		// rather than refusing a file that both read correctly as far as it goes.
		//
		// The reference stops at the first damage. This package steps over damage in the middle of
		// a file and keeps reading whatever is whole after it, so on such a file it holds more than
		// the reference does: the comparison is then of the stretch the reference could read, and
		// what lies past the damage is held to the invariants instead, which need no second decoder.
		if file.Cut == "" {
			t.Fatalf("the reference decoder could not read this file: %v", err)
		}
		damaged = true
		t.Logf("the reference stopped at damage this package recorded (%s): %v", file.Cut, err)
	}
	if len(theirs) == 0 {
		t.Fatal("the reference decoder produced no series")
	}

	// The two spell a path differently and neither spelling can simply be converted into the
	// other: the reference joins with dots, this package with slashes, and a segment can contain
	// either — "local.oplog.rs" is one collection and "number of key/value pairs" is one metric.
	// Stripping both separators is what lines them up.
	flatten := strings.NewReplacer("/", "", ".", "")
	ours := make(map[string][]float64, len(mine.Values))
	for path, values := range mine.Values {
		ours[flatten.Replace(path)] = values
	}
	// A BSON timestamp is two numbers. The reference calls them "x" and "x.inc"; this package calls
	// them "x/t" and "x/i". Same pair, and neither name is wrong.
	// scale is what to multiply this package's value by before comparing. A BSON timestamp's first
	// half is Unix seconds, which is what this package stores and what MongoDB means by it; the
	// reference renders it as a date and so reports milliseconds. Neither is wrong and they are a
	// thousand apart.
	lookup := func(key string) ([]float64, int64, bool) {
		if values, ok := ours[flatten.Replace(key)]; ok {
			return values, 1, true
		}
		if stem, found := strings.CutSuffix(key, ".inc"); found {
			values, ok := ours[flatten.Replace(stem)+"i"]
			return values, 1, ok
		}
		if _, isPair := theirs[key+".inc"]; isPair {
			// The seconds half of a BSON timestamp, which the reference renders as a date and
			// gets wrong: it adds the delta between samples to the first date in milliseconds, so
			// an optime that advanced two seconds comes back two milliseconds later, and the
			// error grows with every sample. This package's plain seconds track the sample clock,
			// which is what an optime does. The increment half, below, matches exactly.
			return nil, 1, false
		}
		return nil, 1, false
	}

	// Doubles are excluded by construction, not by accident. The server truncates a NumberDouble to
	// an integer before the delta stream; this reference keeps the IEEE-754 bit pattern. The two
	// therefore disagree on every double in every file, and the disagreement is not subtle — the
	// reference's reading of systemMetrics/cpu/idle_ms advances by a few milliseconds a second and
	// makes the difference up in one jump per chunk.
	// Comparing them here would assert the reference is right about a thing it is wrong about.
	doubles := make(map[string]bool, 64)
	for path, kind := range mine.Kinds {
		if kind == bsontype.Double.String() {
			doubles[flatten.Replace(path)] = true
		}
	}

	paths := make([]string, 0, len(theirs))
	for path := range theirs {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	compared, mismatched, missing, empty, approximate, incomparable, diverged, narrowed := 0, 0, 0, 0, 0, 0, 0, 0
	for _, key := range paths {
		if doubles[flatten.Replace(key)] {
			diverged++
			continue
		}
		want := theirs[key]
		if len(want) == 0 {
			empty++ // a type the reference did not turn into numbers either
			continue
		}
		got, scale, ok := lookup(key)
		if !ok {
			if _, isPair := theirs[key+".inc"]; isPair {
				incomparable++
				continue
			}
			missing++
			if missing <= 5 {
				t.Errorf("the reference decoded %q and this package did not", key)
			}
			continue
		}
		// A metric that is absent from some chunks — a tmpfs mount that comes and goes with a
		// login, a collection dropped halfway through — is padded here with gaps so that every
		// series shares one time axis, and is emitted by the reference only for the samples it
		// existed in. So the comparison is against this package's real values, in order.
		present := got[:0:0]
		for _, value := range got {
			if !math.IsNaN(value) {
				present = append(present, value)
			}
		}
		if damaged && len(present) > len(want) {
			present = present[:len(want)]
		}
		if len(present) != len(want) {
			mismatched++
			if mismatched <= 5 {
				t.Errorf("%s: %d values, the reference has %d", key, len(present), len(want))
			}
			continue
		}
		compared++
		for i := range want {
			// Either side can be too large for a float64 to hold exactly. The reference's own
			// figure is checked here; this package's is checked below, because an int32 column
			// that accumulated past 2^53 cannot be narrowed back reliably either.
			if want[i] > exactFloat || want[i] < -exactFloat ||
				present[i] > exactFloat || present[i] < -exactFloat {
				approximate++
				break
			}
			got := int64(present[i]) * scale
			if got == want[i] {
				continue
			}
			// The reference rebuilds the document, so a column the chunk declared as an int32
			// comes back rendered as one — truncated, and negative once it has accumulated past
			// 2^31. This package carries the accumulated value whole, because the declared type
			// says how the value was written and not how wide the thing being counted is. The two
			// agree on every bit; they disagree only on that rendering, so the rendering is
			// applied here rather than asserted away.
			if int64(int32(uint32(uint64(got)))) == want[i] {
				narrowed++
				continue
			}
			mismatched++
			if mismatched <= 5 {
				t.Errorf("%s value %d = %v, the reference says %d", key, i, present[i], want[i])
			}
			break
		}
	}
	fmt.Printf("  %-42s %5d metrics × %5d samples agree; missing %d, mismatched %d, beyond float64 %d, not numeric %d, timestamps %d, doubles %d, int32-narrowed %d\n",
		filepathBase(path), compared, len(mine.Timestamps), missing, mismatched, approximate, empty, incomparable, diverged, narrowed)
}

// numberOf reads a reference value as the number this package would have stored for it.
func numberOf(value *birch.Value) (int64, bool) {
	if value == nil {
		return 0, false
	}
	if n, ok := value.Int64OK(); ok {
		return n, true
	}
	if n, ok := value.Int32OK(); ok {
		return int64(n), true
	}
	if n, ok := value.DoubleOK(); ok {
		return int64(n), true
	}
	if b, ok := value.BooleanOK(); ok {
		if b {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// cpuStates are the /proc/stat buckets a second of every core is divided between. guest and
// guest_nice are left out on purpose: the kernel already counts them inside user and nice, and
// adding them again would make a correct decoder look like it was over-counting.
var cpuStates = []string{"user_ms", "nice_ms", "system_ms", "idle_ms", "iowait_ms",
	"irq_ms", "softirq_ms", "steal_ms"}

// The one invariant no decoder-against-decoder test can give: a host spends exactly one second of
// cpu time per core per second, whatever it spends it on. Summed across the states, the counters
// have to advance at the speed of the clock.
//
// Both tests beside this one compare against github.com/mongodb/ftdc, which disagrees with the
// server about doubles, and systemMetrics/cpu is written as doubles. So neither could see that
// every sample inside a chunk was wrong — the chunk's own first sample was right, and a sum over a
// whole file came out correct because its endpoints are two of those. This measures the step
// between one sample and the next, which is the thing that was broken and the thing a chart draws.
func TestCpuCountersAdvanceAtTheSpeedOfTheClock(t *testing.T) {
	root := os.Getenv("DBA_FTDC_FILE")
	if root == "" {
		t.Skip("set DBA_FTDC_FILE to a diagnostic data file or directory written by a real server")
	}
	for _, path := range ftdcFiles(t, root) {
		t.Run(filepathBase(path), func(t *testing.T) {
			file, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := file.Read(time.Time{}, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			cores := decoded.Values["systemMetrics/cpu/num_logical_cores"]
			if len(cores) == 0 || cores[0] <= 0 {
				t.Skip("this file carries no systemMetrics/cpu: nothing to check")
			}
			ratios := perSecondPerCore(decoded, cores[0])
			if len(ratios) < 30 {
				t.Skip("too few samples to have a median worth testing")
			}
			sort.Float64s(ratios)
			median := ratios[len(ratios)/2]
			// A thousand milliseconds per core per second, give or take the sampling jitter.
			if median < 800 || median > 1200 {
				t.Errorf("cpu counters advance by %.0f ms per core-second, want about 1000 on a %.0f-core host",
					median, cores[0])
			}
		})
	}
}

// perSecondPerCore is how much cpu time the counters claim per core, for each step between two
// samples. Steps over a gap in the capture are dropped: a restart leaves a jump that is real.
func perSecondPerCore(decoded *Samples, cores float64) []float64 {
	series := make([][]float64, 0, len(cpuStates))
	for _, state := range cpuStates {
		if values, ok := decoded.Values["systemMetrics/cpu/"+state]; ok {
			series = append(series, values)
		}
	}
	if len(series) == 0 {
		return nil
	}
	ratios := make([]float64, 0, len(decoded.Timestamps))
	for i := 1; i < len(decoded.Timestamps); i++ {
		seconds := float64(decoded.Timestamps[i]-decoded.Timestamps[i-1]) / 1000
		if seconds <= 0 || seconds > 5 {
			continue
		}
		total, ok := 0.0, true
		for _, values := range series {
			step := values[i] - values[i-1]
			if math.IsNaN(step) || step < 0 {
				ok = false
				break
			}
			total += step
		}
		if ok {
			ratios = append(ratios, total/seconds/cores)
		}
	}
	return ratios
}
