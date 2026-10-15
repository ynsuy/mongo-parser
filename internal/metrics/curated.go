package metrics

import "strings"

// Curated reports whether a path is one of the metrics this tool knows by name: the ones that are
// decoded by default, named before everything else, and grouped into families. It is a list, not a
// judgement — which of them matter in a bundle is for the labels to say.
func Curated(path string) bool {
	lower := strings.ToLower(path)
	switch {
	case strings.HasPrefix(path, "serverStatus/wiredTiger/concurrentTransactions/") && (strings.HasSuffix(path, "/out") || strings.HasSuffix(path, "/available")):
		return true
	case ticketReading(path):
		return true
	case strings.HasPrefix(path, "serverStatus/globalLock/currentQueue/"):
		return true
	// Flow control is the one mechanism that makes writes slow on an idle machine: when the
	// majority commit point falls behind, the primary throttles writers on purpose. targetRateLimit
	// sits at a billion until that happens, so it costs a flat line to carry and is the whole
	// answer on the bundles where it is not flat.
	case path == "serverStatus/flowControl/targetRateLimit":
		return true
	case path == "serverStatus/flowControl/isLagged", path == "serverStatus/flowControl/isLaggedCount",
		path == "serverStatus/flowControl/isLaggedTimeMicros":
		return true
	case path == "serverStatus/flowControl/sustainerRate", path == "serverStatus/flowControl/locksPerKiloOp":
		return true
	case path == "serverStatus/wiredTiger/cache/tracked dirty bytes in the cache":
		return true
	// Memory tcmalloc is holding but the application is not using. A node whose resident set will
	// not come down is usually this and not a leak, and it is the first number to look at when the
	// machine is short of memory and mongod says it is not using it.
	case path == "serverStatus/tcmalloc/tcmalloc/pageheap_free_bytes":
		return true
	case path == "serverStatus/tcmalloc/generic/current_allocated_bytes",
		path == "serverStatus/tcmalloc/generic/heap_size":
		return true
	case strings.HasPrefix(path, "serverStatus/opLatencies/"):
		return true
	case path == "serverStatus/wiredTiger/cache/bytes currently in the cache":
		return true
	case strings.Contains(lower, "systemmetrics/cpu/iowait"):
		return true
	// Checkpoint duration and occupancy matter; the boolean bookkeeping flags around them do not.
	case strings.Contains(lower, "checkpoint") && (strings.Contains(lower, "msecs") || strings.Contains(lower, "currently running") || strings.HasSuffix(lower, "checkpoints")):
		return true
	case strings.HasPrefix(path, "serverStatus/opcounters/"):
		return true
	case strings.HasPrefix(path, "systemMetrics/disks/") && strings.Contains(path, "io_time"):
		return true
	case path == "serverStatus/extra_info/page_faults":
		return true
	case strings.HasPrefix(path, "systemMetrics/disks/") && (strings.Contains(path, "read_time") || strings.Contains(path, "write_time")):
		return true
	case strings.HasPrefix(path, "systemMetrics/cpu/"):
		return true
	case strings.HasPrefix(path, "serverStatus/globalLock/activeClients/"):
		return true
	case path == "serverStatus/connections/current":
		return true
	case strings.HasSuffix(path, "timeAcquiringMicros") || strings.HasSuffix(path, "acquireWaitCount"):
		return true
	case strings.Contains(lower, "pages evicted") || strings.Contains(lower, "pages queued for eviction") ||
		strings.Contains(lower, "evicting pages"):
		return true
	case strings.HasPrefix(path, "serverStatus/metrics/queryExecutor/"):
		return true
	case strings.HasPrefix(path, "serverStatus/metrics/repl/"):
		return true
	case strings.HasPrefix(path, "serverStatus/opcountersRepl/"):
		return true
	case strings.Contains(lower, "pages read into cache") || strings.Contains(lower, "pages written from cache"):
		return true
	case strings.HasPrefix(path, "serverStatus/network/"):
		return true
	case path == "serverStatus/mem/resident":
		return true
	case path == "serverStatus/tcmalloc/tcmalloc/total_free_bytes",
		path == "serverStatus/tcmalloc/tcmalloc/pageheap_unmapped_bytes":
		return true
	case strings.HasPrefix(path, "serverStatus/tcmalloc/"):
		return true
	case strings.HasPrefix(path, "systemMetrics/memory/"):
		return true
	case path == "serverStatus/connections/available":
		return true
	// Refused connections, and the rate new ones open: a client that cannot get in, and a client
	// reconnecting in a loop, are both invisible in the current count.
	case path == "serverStatus/connections/rejected":
		return true
	case path == "serverStatus/connections/totalCreated":
		return true
	case path == "serverStatus/wiredTiger/cache/maximum bytes configured":
		return true
	case strings.HasPrefix(path, "systemMetrics/disks/") && (strings.HasSuffix(path, "/reads") || strings.HasSuffix(path, "/writes")):
		return true
	// The network under the server. Retransmissions and a full accept queue are what a slow
	// replica set looks like from the kernel, and mongod has no counter of its own for either.
	case path == netstat+"Tcp:RetransSegs":
		return true
	case path == netstat+"TcpExt:TCPTimeouts", path == netstat+"TcpExt:ListenOverflows",
		path == netstat+"TcpExt:ListenDrops":
		return true
	case path == netstat+"Tcp:EstabResets", path == netstat+"Tcp:OutRsts", path == netstat+"Tcp:AttemptFails",
		path == netstat+"Tcp:InErrs", path == netstat+"TcpExt:TCPAbortOnTimeout":
		return true
	case path == netstat+"Tcp:CurrEstab":
		return true
	// The divisor of the retransmission share.
	case path == netstat+"Tcp:OutSegs", path == netstat+"Tcp:InSegs":
		return true
	}
	return false
}

// retransmitWarning is the share of segments retransmitted over a minute that is worth a look. A
// healthy LAN retransmits a small fraction of a percent; one percent sustained for a minute is

// Overview is the standard board as a flat list of metrics, for the commands that draw one metric
// per row rather than several to a panel.
//
// It is derived from the overview board rather than curated a second time. Two hand-written lists
// of "the metrics that matter" drift, and then the board a person reads and the board a program is
// handed stop being the same board — which is the one thing that must not happen when the program
// is summarising the bundle for someone who is looking at it.
//
// At most two lines are taken from each panel. A flat list a dozen long that spent six of them on
// opcounters would answer one question well and five not at all; the panels are already in the
// order the questions are asked, so breadth is a matter of taking the top of each.
func Overview(available []string) []string {
	const perPanel = 2
	seen := map[string]bool{}
	out := make([]string, 0, 16)
	for _, board := range Boards() {
		if board.Name != OverviewBoard {
			continue
		}
		for _, panel := range board.Panels {
			taken := 0
			for _, line := range panel.Resolve(available) {
				if taken == perPanel {
					break
				}
				if seen[line.Path] {
					continue
				}
				seen[line.Path] = true
				out = append(out, line.Path)
				taken++
			}
		}
	}
	return out
}

// Family is the group a metric belongs to. Metrics that answer the same question belong on one
// screen: tickets next to queues says whether work was waiting, and a cache number on its own says
// very little. An uncurated path has no family.
func Family(path string) string {
	lower := strings.ToLower(path)
	switch {
	case !Curated(path):
		return ""
	case strings.Contains(path, "concurrentTransactions"), strings.HasPrefix(path, executionQueue):
		return "tickets"
	// Per-collection storage stats are a different subject from the server's own numbers: they say
	// what one namespace did, not what the node did, and there are hundreds of them.
	case strings.Contains(path, ".stats/storageStats/"):
		return "collections"
	case strings.HasPrefix(path, "serverStatus/globalLock/"),
		strings.HasPrefix(path, "serverStatus/flowControl/"):
		return "queue"
	case strings.Contains(lower, "checkpoint"):
		return "checkpoint"
	case strings.HasPrefix(path, "serverStatus/wiredTiger/cache/"), strings.Contains(lower, "evict"),
		strings.Contains(lower, "pages read into cache"), strings.Contains(lower, "pages written from cache"):
		return "cache"
	case strings.HasPrefix(path, "serverStatus/opLatencies/"):
		return "latency"
	case strings.HasPrefix(path, "serverStatus/opcounters"), strings.HasPrefix(path, "serverStatus/metrics/queryExecutor/"):
		return "operations"
	case strings.HasPrefix(path, "serverStatus/metrics/repl/"):
		return "replication"
	case strings.HasPrefix(path, "systemMetrics/disks/"):
		return "disk"
	case strings.HasPrefix(path, "systemMetrics/cpu/"):
		return "cpu"
	case strings.HasPrefix(path, "serverStatus/tcmalloc/"):
		return "heap"
	case strings.HasPrefix(path, "systemMetrics/memory/"), path == "serverStatus/mem/resident",
		path == "serverStatus/extra_info/page_faults":
		return "memory"
	case strings.HasPrefix(path, "serverStatus/connections/"):
		return "connections"
	case strings.HasPrefix(path, "serverStatus/network/"), strings.HasPrefix(path, netstat):
		return "network"
	}
	return "other"
}

// Families are the groups in the order they are worth looking at: what work was waiting on first,
// then what the server was doing, then the machine under it.
func Families() []string {
	return []string{
		"tickets", "queue", "latency", "operations", "cache", "checkpoint",
		"replication", "connections", "network", "cpu", "disk", "memory", "heap",
		"collections", "other",
	}
}
