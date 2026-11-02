package label

import (
	"testing"
)

// One busy disk among idle ones is concentrated; disks that all idle are not compared.
func TestOneHotDiskIsNotAlike(t *testing.T) {
	paths := map[string]func(int) float64{}
	for dev, busy := range map[string]float64{"sda": 900, "sdb": 50, "sdc": 40, "sdd": 45} {
		paths["systemMetrics/disks/"+dev+"/io_time_ms"] = counter(func(int) float64 { return busy })
	}
	found := Imbalances(run(samplesFor(1800, paths), "n"), nil)
	for _, im := range found {
		if im.Label == "disk.busy" {
			if im.Hottest != "sda" || im.Comparison != "share_80pct_or_more" || im.Source != "n" || im.Count != 4 {
				t.Fatalf("%+v", im)
			}
			return
		}
	}
	t.Fatalf("no disk.busy imbalance among %+v", found)
}

// A primary busier than its secondaries is a replica set doing its job; one secondary busier than the
// other is not, and a node that changed state partway is compared with nothing.
func TestNodesAreComparedWithPeersInTheSameRole(t *testing.T) {
	busy := map[string]float64{"a": 90, "b": 10, "c": 40, "d": 95}
	var results []Result
	for _, node := range []string{"a", "b", "c", "d"} {
		results = append(results, run(samplesFor(1800, cpuAt(func(int) float64 { return busy[node] })), node)...)
	}
	peers := map[string]string{"a": "mongod PRIMARY", "b": "mongod SECONDARY", "c": "mongod SECONDARY", "d": PeerChanged}
	var cpu []Imbalance
	for _, im := range Imbalances(results, peers) {
		if im.Label == "cpu.busy" {
			cpu = append(cpu, im)
		}
	}
	if len(cpu) != 1 {
		t.Fatalf("want one cpu.busy comparison, among the two secondaries: %+v", cpu)
	}
	if im := cpu[0]; im.Peers != "mongod SECONDARY" || im.Hottest != "c" || im.Count != 2 || im.Comparison != "3x_or_more" {
		t.Fatalf("%+v", im)
	}
}
