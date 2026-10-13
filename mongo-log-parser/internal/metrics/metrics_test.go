package metrics

import (
	"testing"
	"time"
)

func TestAutoBucketSecondsFitsOneBinPerColumn(t *testing.T) {
	for _, c := range []struct {
		span    time.Duration
		sample  int64
		bins    int
		seconds int64
	}{
		{time.Hour, 1, 60, 120},       // a minute each needs 61 columns
		{time.Hour, 1, 61, 60},        // and gets them
		{3 * time.Hour, 1, 60, 300},   // the finest step whose bins fit 60 columns
		{15 * time.Minute, 1, 60, 30}, //
		{24 * time.Hour, 1, 60, 1800}, //
		{time.Hour, 10, 61, 60},       // sampling every 10s still allows minute bins
		{5 * time.Minute, 10, 60, 10}, // short windows stop at the sample interval
		{time.Hour, 1, 100, 60},       //
		{time.Hour, 1, 200, 30},       // a wider plot buys finer bins
	} {
		if got := AutoBucketSecondsFor(c.span, c.sample, c.bins); got != c.seconds {
			t.Errorf("span %s at %ds sampling over %d bins: got %d, want %d", c.span, c.sample, c.bins, got, c.seconds)
		}
	}
}

// Every metric worth charting belongs somewhere. A group screen exists so a number is read next to
// the ones that explain it, and "other" is a bucket, not an answer.
func TestEveryCuratedMetricHasAFamily(t *testing.T) {
	for _, path := range []string{
		"serverStatus/wiredTiger/concurrentTransactions/write/out",
		"serverStatus/globalLock/currentQueue/total",
		"serverStatus/flowControl/timeAcquiringMicros",
		"serverStatus/opLatencies/reads/latency",
		"serverStatus/opcounters/query",
		"serverStatus/metrics/queryExecutor/scanned",
		"serverStatus/wiredTiger/cache/tracked dirty bytes in the cache",
		"serverStatus/wiredTiger/historyStorageStats/cache/pages read into cache",
		"serverStatus/wiredTiger/checkpoint/total time (msecs)",
		"serverStatus/metrics/repl/apply/batches/totalMillis",
		"serverStatus/connections/current",
		"serverStatus/network/bytesIn",
		"systemMetrics/cpu/iowait_ms",
		"systemMetrics/disks/vda/io_time_ms",
		"serverStatus/extra_info/page_faults",
		"config.transactions.stats/storageStats/wiredTiger/cache/pages read into cache",
	} {
		family := Family(path)
		if family == "" || family == "other" {
			t.Errorf("%s landed in %q", path, family)
		}
		known := false
		for _, name := range Families() {
			known = known || name == family
		}
		if !known {
			t.Errorf("%s has family %q, which Families() does not list", path, family)
		}
	}
}

// A path the curation does not rate has no family, so nothing uncurated reaches a group screen.
func TestUncuratedMetricsHaveNoFamily(t *testing.T) {
	if got := Family("serverStatus/somethingNobodyRated"); got != "" {
		t.Errorf("an uncurated path got family %q", got)
	}
}

// Per-collection storage stats are their own subject: hundreds of them, about one namespace each,
// and they must not crowd out the server's own cache numbers.
func TestCollectionStatsAreTheirOwnFamily(t *testing.T) {
	server := "serverStatus/wiredTiger/cache/pages read into cache"
	collection := "config.image_collection.stats/storageStats/wiredTiger/cache/pages read into cache"
	if Family(server) != "cache" {
		t.Errorf("the server's own cache metric landed in %q", Family(server))
	}
	if Family(collection) != "collections" {
		t.Errorf("a per-collection metric landed in %q", Family(collection))
	}
}

// A share against a total the bundle only half carries is not a smaller answer, it is a wrong one:
// every percentage would be inflated by whatever is missing. The panel drops instead.
func TestCPUSharePanelNeedsEveryState(t *testing.T) {
	if lines := cpuSharePanel().Resolve(cpuStates); len(lines) == 0 {
		t.Error("a bundle carrying every state resolved no lines")
	}
	if lines := cpuSharePanel().Resolve(cpuStates[:len(cpuStates)-1]); len(lines) != 0 {
		t.Errorf("a bundle missing a state still resolved %d lines", len(lines))
	}
	// The plain ms-per-second panel is still there for a bundle that cannot make a percentage.
	if lines := cpuPanel().Resolve(cpuStates[:len(cpuStates)-1]); len(lines) == 0 {
		t.Error("the ms-per-second panel went with it")
	}
}
