package cli

import (
	"slices"
	"testing"
)

// The decode filter keeps what the words after the path picked beside the curated set, so a metric
// outside it is decoded the moment someone asks for it rather than reported as not existing.
func TestSelectorDecodesWhatWasPickedBesideTheCuratedSet(t *testing.T) {
	want := selector(nil, []string{"systemMetrics/netstat/IpExt:InOctets"})
	if !want("systemMetrics/netstat/IpExt:InOctets") {
		t.Error("a picked uncurated metric was not decoded")
	}
	if !want("serverStatus/opcounters/insert") {
		t.Error("the curated set was dropped when something else was picked")
	}
	if want("systemMetrics/netstat/IpExt:InBcastPkts") {
		t.Error("an uncurated metric nobody asked for was decoded")
	}
}

// The catalogue lists every metric, the uncurated ones under the subtree they come from, after the
// subjects a triage walks.
func TestCatalogueListsEveryMetric(t *testing.T) {
	groups := catalogue([]string{
		"serverStatus/opcounters/insert",
		"systemMetrics/netstat/Tcp:RetransSegs",
		"systemMetrics/netstat/IpExt:InBcastPkts",
		"serverStatus/wiredTiger/block-manager/blocks read",
	})
	names, total := make([]string, 0, len(groups)), 0
	for _, g := range groups {
		names = append(names, g.Name)
		total += len(g.Paths)
	}
	if total != 4 {
		t.Fatalf("catalogue holds %d metrics, want 4: %v", total, groups)
	}
	want := []string{"operations", "network", "serverStatus/wiredTiger", "systemMetrics/netstat"}
	if !slices.Equal(names, want) {
		t.Errorf("groups = %v, want %v", names, want)
	}
}
