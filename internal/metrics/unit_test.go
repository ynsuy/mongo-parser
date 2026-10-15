package metrics

import (
	"testing"

	"github.com/ynsuy/mongo-parser/internal/series"
)

func TestUnitOfNamesTheBytes(t *testing.T) {
	for path, want := range map[string]string{
		"serverStatus/wiredTiger/cache/bytes currently in the cache": series.UnitBytes,
		"serverStatus/wiredTiger/cache/maximum bytes configured":     series.UnitBytes,
		"serverStatus/tcmalloc/generic/current_allocated_bytes":      series.UnitBytes,
		"serverStatus/tcmalloc/generic/heap_size":                    series.UnitBytes,
		"serverStatus/metrics/repl/network/bytes":                    series.UnitBytes,
		"systemMetrics/memory/MemFree_kb":                            series.UnitKiB,
		"serverStatus/mem/resident":                                  series.UnitMiB,
		"serverStatus/opcounters/query":                              "",
		"systemMetrics/cpu/user_ms":                                  "",
		"serverStatus/globalLock/currentQueue/total":                 "",
	} {
		if got := UnitOf(path); got != want {
			t.Errorf("UnitOf(%q) = %q, want %q", path, got, want)
		}
	}
}
