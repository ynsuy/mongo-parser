package metrics

import (
	"strings"

	"github.com/ynsuy/mongo-parser/internal/series"
)

// A panel is several metrics on one chart, and a board is a screen of panels.
//
// One metric on its own rarely answers anything. Inserts mean little without updates and deletes
// beside them; a read latency means little without the write latency it traded against. Every
// dashboard anyone actually reads is built this way, and this is the same catalogue: a fixed set of
// questions, each drawn as the handful of numbers that answer it, in a fixed order.

// Line is one series on a panel.
type Line struct {
	// Label is what the legend calls it. Empty takes the metric's own short name, which is right
	// when the path already reads well and wrong when four paths differ only in their last segment.
	Label string
	Path  string
	// Alt is the same reading under the names other server versions gave it, tried in order when
	// Path is not in the bundle. MongoDB moves metrics between releases — the concurrency tickets
	// left serverStatus.wiredTiger.concurrentTransactions for serverStatus.queues.execution in 7.0
	// — and a panel that silently disappears because the server renamed its field is worse than no
	// catalogue at all: the reader is told this server does not report tickets, which is false.
	Alt []string
	// Of makes the line a share: the change in Path against the change in all of Of together, as a
	// percentage. It is how CPU becomes a number people quote — FTDC keeps /proc/stat as counters
	// of milliseconds, and their sum over a second is the machine's whole capacity for that second,
	// so the core count never has to be known.
	Of []string
	// Per divides the change in Path by the change in Per. MongoDB reports total microseconds and
	// total operations and neither says how long an operation took; latency is that division, and
	// drawn without it the chart shows a running total in the billions where it means a fraction
	// of a millisecond.
	Per string
}

// Panel is the metrics that belong on one chart, in the order they are drawn and listed. The first
// one keeps a cell where two lines cross, so the number worth seeing goes first.
type Panel struct {
	Name  string
	Lines []Line
	// Shared asks for one scale across the panel rather than each band filling to its own peak.
	//
	// It is for panels whose metrics are the same unit and where the proportion between them is the
	// question: how much of the heap is free against how much is in use, how much of the cache is
	// configured against how much is used. On a panel of unrelated magnitudes — six operation
	// counters — sharing would leave the small ones as hairlines, so it stays off by default and
	// this is the exception. The reader's key inverts whatever the panel asked for.
	Shared bool
	// Match adds every path it accepts, for the families the machine names: a line per disk, per
	// network device, per CPU. A bundle from a host with four disks gets four lines; one with one
	// gets one, and neither needs a catalogue entry naming the device.
	Match func(string) bool
	// MatchLabel shortens a matched path to something a legend can hold.
	MatchLabel func(string) string
}

// Board is one screen of panels.
type Board struct {
	Name   string
	Panels []Panel
}

const (
	// tcm is the tcmalloc subtree. serverStatus prints it as a block of MALLOC: lines with a
	// formatted string beside them; FTDC keeps only numbers, so the string is not in the file and
	// these paths are. They are the same figures, once a second, which is finer than the text
	// version anyone has ever read out of a log.
	tcm   = "serverStatus/tcmalloc/"
	wt    = "serverStatus/wiredTiger/"
	lock  = "serverStatus/globalLock/"
	lat   = "serverStatus/opLatencies/"
	ops   = "serverStatus/opcounters/"
	repl  = "serverStatus/metrics/repl/"
	cpu   = "systemMetrics/cpu/"
	disks = "systemMetrics/disks/"
	// netstat is /proc/net/snmp and /proc/net/netstat, one counter per field, keyed "Tcp:RetransSegs".
	netstat = "systemMetrics/netstat/"
	// queue is where 7.0 moved the concurrency tickets that used to live under wiredTiger.
	queue = "serverStatus/queues/execution/"
	flow  = "serverStatus/flowControl/"
)

// OverviewBoard is the board a session opens on: the questions a triage asks first, in order.
const OverviewBoard = "overview"

// Boards is the catalogue, in the order a triage walks it: what the server was being asked to do,
// then what the storage engine did about it, then what the machine underneath was doing.
func Boards() []Board {
	return []Board{
		{Name: OverviewBoard, Panels: []Panel{
			queuePanel(), ticketsPanel(), latencyPanel(), operationsPanel(),
			cacheSizePanel(), cpuSharePanel(), cpuPanel(), connectionsPanel(), heapPanel(), diskTimePanel(),
		}},
		{Name: "wiredtiger", Panels: []Panel{
			ticketsPanel(), ticketPoolPanel(), ticketQueuePanel(), cacheSizePanel(), dirtyCachePanel(),
			evictionPanel(), cacheIOPanel(), checkpointPanel(),
		}},
		{Name: "operations", Panels: []Panel{
			operationsPanel(), latencyPanel(), opsPerSecondPanel(), queryExecutorPanel(),
			queuePanel(), activeClientsPanel(), flowControlPanel(), connectionsPanel(),
		}},
		{Name: "replication", Panels: []Panel{
			flowControlPanel(), flowControlLagPanel(),
			{Name: "Replicated operations", Lines: []Line{
				{Label: "insert", Path: "serverStatus/opcountersRepl/insert"},
				{Label: "update", Path: "serverStatus/opcountersRepl/update"},
				{Label: "delete", Path: "serverStatus/opcountersRepl/delete"},
				{Label: "command", Path: "serverStatus/opcountersRepl/command"},
				{Label: "getmore", Path: "serverStatus/opcountersRepl/getmore"},
				{Label: "query", Path: "serverStatus/opcountersRepl/query"},
			}},
			{Name: "Apply", Lines: []Line{
				{Label: "ops", Path: repl + "apply/ops"},
				{Label: "batches", Path: repl + "apply/batches/num"},
				{Label: "ms per batch", Path: repl + "apply/batches/totalMillis", Per: repl + "apply/batches/num"},
			}},
			{Name: "Oplog fetch", Lines: []Line{
				{Label: "bytes", Path: repl + "network/bytes"},
				{Label: "ops", Path: repl + "network/ops"},
				{Label: "ms per getmore", Path: repl + "network/getmores/totalMillis", Per: repl + "network/getmores/num"},
			}},
			{Name: "Apply buffer", Lines: []Line{
				{Label: "bytes", Path: repl + "buffer/sizeBytes"},
				{Label: "max bytes", Path: repl + "buffer/maxSizeBytes"},
				{Label: "count", Path: repl + "buffer/count"},
			}},
		}},
		{Name: "heap", Panels: []Panel{
			heapPanel(), freelistPanel(),
			{Name: "Page heap activity (per second)", Lines: []Line{
				{Label: "commits", Path: tcm + "tcmalloc/pageheap_commit_count"},
				{Label: "decommits", Path: tcm + "tcmalloc/pageheap_decommit_count"},
				{Label: "scavenges", Path: tcm + "tcmalloc/pageheap_scavenge_count"},
				{Label: "reserves", Path: tcm + "tcmalloc/pageheap_reserve_count"},
			}},
			{Name: "Page heap bytes moved (per second)", Lines: []Line{
				{Label: "committed", Path: tcm + "tcmalloc/pageheap_total_commit_bytes"},
				{Label: "decommitted", Path: tcm + "tcmalloc/pageheap_total_decommit_bytes"},
				{Label: "reserved", Path: tcm + "tcmalloc/pageheap_total_reserve_bytes"},
			}},
			{Name: "Allocator contention (ns per second)", Lines: []Line{
				{Label: "spinlock delay", Path: tcm + "tcmalloc/spinlock_total_delay_ns"},
			}},
			memoryPanel(),
		}},
		{Name: "system", Panels: []Panel{
			cpuSharePanel(), cpuPanel(), diskTimePanel(), diskLatencyPanel(), memoryPanel(),
			{Name: "Page faults", Lines: []Line{{Label: "faults", Path: "serverStatus/extra_info/page_faults"}}},
			networkPanel(), tcpRetransmitPanel(), tcpConnectionsPanel(),
			{Name: "Runnable and blocked", Lines: []Line{
				{Label: "running", Path: cpu + "procs_running"},
				{Label: "blocked", Path: cpu + "procs_blocked"},
			}},
		}},
	}
}

func queuePanel() Panel {
	return Panel{Name: "Queued operations", Lines: []Line{
		{Label: "total", Path: lock + "currentQueue/total"},
		{Label: "readers", Path: lock + "currentQueue/readers"},
		{Label: "writers", Path: lock + "currentQueue/writers"},
	}}
}

// flowControlPanel is the primary throttling its own writers on purpose.
//
// It is the one mechanism that makes writes slow while the machine is idle: when the majority
// commit point falls behind, the primary holds writers back so the secondaries can catch up.
// targetRateLimit sits at a billion — the server's way of writing "no limit" — until that happens,
// which is why it is worth a panel that is a flat line on a healthy node.
func flowControlPanel() Panel {
	return Panel{Name: "Flow control", Lines: []Line{
		{Label: "target rate limit", Path: flow + "targetRateLimit"},
		{Label: "sustainer rate", Path: flow + "sustainerRate"},
		{Label: "µs acquiring (per second)", Path: flow + "timeAcquiringMicros"},
	}}
}

// flowControlLagPanel is how often and how long the mechanism considered the set lagged, which is
// the cause behind the throttling the panel above shows the effect of.
func flowControlLagPanel() Panel {
	return Panel{Name: "Flow control lag", Lines: []Line{
		{Label: "lagged", Path: flow + "isLagged"},
		{Label: "became lagged (per second)", Path: flow + "isLaggedCount"},
		{Label: "µs lagged (per second)", Path: flow + "isLaggedTimeMicros"},
		{Label: "locks per kilo-op", Path: flow + "locksPerKiloOp"},
	}}
}

func activeClientsPanel() Panel {
	return Panel{Name: "Active clients", Lines: []Line{
		{Label: "total", Path: lock + "activeClients/total"},
		{Label: "readers", Path: lock + "activeClients/readers"},
		{Label: "writers", Path: lock + "activeClients/writers"},
	}}
}

// ticketsPanel is the concurrency tickets a node is holding: the number that says whether the
// storage engine is the bottleneck.
//
// Both names are listed. The counters lived under wiredTiger.concurrentTransactions up to 6.x, moved
// to queues.execution in 7.0, and 8.0 dropped the old spelling — so a catalogue that knows only one
// of them drops the panel on half the bundles it is given.
func ticketsPanel() Panel {
	return Panel{Name: "Tickets in use", Lines: []Line{
		{Label: "read", Path: wt + "concurrentTransactions/read/out", Alt: []string{queue + "read/out"}},
		{Label: "write", Path: wt + "concurrentTransactions/write/out", Alt: []string{queue + "write/out"}},
	}}
}

// ticketPoolPanel is how much of the pool is left, against how big the pool is. 7.0 sizes the pool
// dynamically, so "how many are left" cannot be read without the total beside it.
func ticketPoolPanel() Panel {
	return Panel{Name: "Tickets left", Shared: true, Lines: []Line{
		{Label: "read left", Path: wt + "concurrentTransactions/read/available", Alt: []string{queue + "read/available"}},
		{Label: "write left", Path: wt + "concurrentTransactions/write/available", Alt: []string{queue + "write/available"}},
		{Label: "read pool", Path: wt + "concurrentTransactions/read/totalTickets", Alt: []string{queue + "read/totalTickets"}},
		{Label: "write pool", Path: wt + "concurrentTransactions/write/totalTickets", Alt: []string{queue + "write/totalTickets"}},
	}}
}

// ticketQueuePanel is what is waiting for a ticket, which 7.0 reports and 6.x does not.
//
// 8.0 split the queue by admission priority: there is no queueLength beside the pool any more, only
// one per bucket. normalPriority is the bucket ordinary work is admitted in, so it is the same
// reading under a longer name, and without it the panel is empty on every 8.0 bundle.
func ticketQueuePanel() Panel {
	return Panel{Name: "Waiting for a ticket", Lines: []Line{
		{Label: "read queued", Path: queue + "read/queueLength", Alt: []string{queue + "read/normalPriority/queueLength"}},
		{Label: "write queued", Path: queue + "write/queueLength", Alt: []string{queue + "write/normalPriority/queueLength"}},
	}}
}

// latencyPanel is microseconds per operation, not the microseconds counter. The counter climbs into
// the billions and says nothing; the division is the number a person means by "latency".
func latencyPanel() Panel {
	return Panel{Name: "Latency per operation (µs)", Lines: []Line{
		{Label: "reads", Path: lat + "reads/latency", Per: lat + "reads/ops"},
		{Label: "writes", Path: lat + "writes/latency", Per: lat + "writes/ops"},
		{Label: "commands", Path: lat + "commands/latency", Per: lat + "commands/ops"},
	}}
}

func opsPerSecondPanel() Panel {
	return Panel{Name: "Operations measured for latency", Lines: []Line{
		{Label: "reads", Path: lat + "reads/ops"},
		{Label: "writes", Path: lat + "writes/ops"},
		{Label: "commands", Path: lat + "commands/ops"},
	}}
}

func operationsPanel() Panel {
	return Panel{Name: "Operations", Lines: []Line{
		{Label: "query", Path: ops + "query"},
		{Label: "insert", Path: ops + "insert"},
		{Label: "update", Path: ops + "update"},
		{Label: "delete", Path: ops + "delete"},
		{Label: "getmore", Path: ops + "getmore"},
		{Label: "command", Path: ops + "command"},
	}}
}

func queryExecutorPanel() Panel {
	return Panel{Name: "Documents and keys examined", Lines: []Line{
		{Label: "keys", Path: "serverStatus/metrics/queryExecutor/scanned"},
		{Label: "documents", Path: "serverStatus/metrics/queryExecutor/scannedObjects"},
	}}
}

func connectionsPanel() Panel {
	return Panel{Name: "Connections", Shared: true, Lines: []Line{
		{Label: "current", Path: "serverStatus/connections/current"},
		{Label: "available", Path: "serverStatus/connections/available"},
	}}
}

func cacheSizePanel() Panel {
	return Panel{Name: "Cache", Shared: true, Lines: []Line{
		{Label: "in cache", Path: wt + "cache/bytes currently in the cache"},
		{Label: "configured", Path: wt + "cache/maximum bytes configured"},
	}}
}

func dirtyCachePanel() Panel {
	return Panel{Name: "Dirty cache", Lines: []Line{
		{Label: "dirty bytes", Path: wt + "cache/tracked dirty bytes in the cache"},
	}}
}

func evictionPanel() Panel {
	return Panel{Name: "Pages evicted", Lines: []Line{
		{Label: "modified", Path: wt + "cache/modified pages evicted"},
		{Label: "unmodified", Path: wt + "cache/unmodified pages evicted"},
		{Label: "by app threads", Path: wt + "cache/pages evicted by application threads"},
		{Label: "queued", Path: wt + "cache/pages queued for eviction"},
	}}
}

func cacheIOPanel() Panel {
	return Panel{Name: "Cache pages in and out", Lines: []Line{
		{Label: "read in", Path: wt + "cache/pages read into cache"},
		{Label: "written out", Path: wt + "cache/pages written from cache"},
	}}
}

func checkpointPanel() Panel {
	return Panel{Name: "Checkpoint (ms)", Lines: []Line{
		{Label: "most recent", Path: wt + "transaction/transaction checkpoint most recent time (msecs)"},
		{Label: "max", Path: wt + "transaction/transaction checkpoint max time (msecs)"},
		{Label: "running", Path: wt + "transaction/transaction checkpoint currently running"},
	}}
}

func cpuPanel() Panel {
	return Panel{Name: "CPU (ms per second)", Lines: []Line{
		{Label: "user", Path: cpu + "user_ms"},
		{Label: "system", Path: cpu + "system_ms"},
		{Label: "iowait", Path: cpu + "iowait_ms"},
		{Label: "steal", Path: cpu + "steal_ms"},
	}}
}

// cpuStates are every way /proc/stat accounts for a tick of CPU time. Their sum over a second is
// the machine's whole capacity for that second, whatever its core count, which is what makes a
// percentage possible from FTDC alone.
//
// guest_ms and guest_nice_ms are left out on purpose: Linux already counts guest time inside
// user_ms and nice_ms, so adding them would inflate the total and quietly shrink every percentage.
var cpuStates = []string{
	cpu + "user_ms", cpu + "nice_ms", cpu + "system_ms", cpu + "idle_ms",
	cpu + "iowait_ms", cpu + "irq_ms", cpu + "softirq_ms", cpu + "steal_ms",
}

// cpuSharePanel is CPU as the number people quote: a percentage of the machine, not milliseconds
// per second. The lines are the states worth telling apart — time in the application, time in the
// kernel, time waiting on a disk, and time a hypervisor took away.
func cpuSharePanel() Panel {
	return Panel{Name: "CPU (% of the machine)", Shared: true, Lines: []Line{
		{Label: "user", Path: cpu + "user_ms", Of: cpuStates},
		{Label: "system", Path: cpu + "system_ms", Of: cpuStates},
		{Label: "iowait", Path: cpu + "iowait_ms", Of: cpuStates},
		{Label: "steal", Path: cpu + "steal_ms", Of: cpuStates},
		{Label: "nice", Path: cpu + "nice_ms", Of: cpuStates},
		{Label: "idle", Path: cpu + "idle_ms", Of: cpuStates},
	}}
}

// carriedAll reports whether the bundle has every one of these paths.
func carriedAll(carried map[string]bool, paths []string) bool {
	for _, path := range paths {
		if !carried[path] {
			return false
		}
	}
	return true
}

// heapPanel is the MALLOC: block of serverStatus, as numbers.
//
// The line that matters is the page heap freelist: memory tcmalloc has taken from the machine and
// is holding, which the application is not using. A node whose resident set will not come down is
// usually this rather than a leak, and reading it beside what the application actually has is the
// whole question — so this panel shares one scale.
//
// The labels are the wording serverStatus prints, not the field names, so a reader comparing the
// chart to a MALLOC: block in a log or a ticket does not have to translate between them.
func heapPanel() Panel {
	return Panel{Name: "Heap (tcmalloc)", Shared: true, Lines: []Line{
		{Label: "app in use", Path: tcm + "generic/current_allocated_bytes"},
		{Label: "pageheap free", Path: tcm + "tcmalloc/pageheap_free_bytes"},
		{Label: "unmapped", Path: tcm + "tcmalloc/pageheap_unmapped_bytes"},
		{Label: "heap size", Path: tcm + "generic/heap_size"},
	}}
}

// freelistPanel is where the free bytes are being held: stuck in the page heap, or spread across
// the per-thread and shared caches.
//
// total_free_bytes is deliberately not on it. It can read smaller than the page heap freelist
// alone, so whatever it totals it is not everything, and a band labelled "total free" that is
// smaller than one of its own parts is a wrong reading rather than an incomplete one.
// The four components below add up to the answer; the field stays reachable by --metric.
func freelistPanel() Panel {
	return Panel{Name: "Free lists (tcmalloc)", Shared: true, Lines: []Line{
		{Label: "pageheap", Path: tcm + "tcmalloc/pageheap_free_bytes"},
		{Label: "thread cache", Path: tcm + "tcmalloc/thread_cache_free_bytes"},
		{Label: "central cache", Path: tcm + "tcmalloc/central_cache_free_bytes"},
		{Label: "transfer cache", Path: tcm + "tcmalloc/transfer_cache_free_bytes"},
	}}
}

func memoryPanel() Panel {
	return Panel{Name: "Memory", Lines: []Line{
		{Label: "resident", Path: "serverStatus/mem/resident"},
		{Label: "free kb", Path: "systemMetrics/memory/MemFree_kb"},
		{Label: "cached kb", Path: "systemMetrics/memory/Cached_kb"},
		{Label: "dirty kb", Path: "systemMetrics/memory/Dirty_kb"},
	}}
}

func networkPanel() Panel {
	return Panel{Name: "Network", Lines: []Line{
		{Label: "in", Path: "serverStatus/network/bytesIn"},
		{Label: "out", Path: "serverStatus/network/bytesOut"},
		{Label: "requests", Path: "serverStatus/network/numRequests"},
	}}
}

// tcpRetransmitPanel is whether the network under the server was losing packets. A retransmission
// is a segment the kernel sent again because the first copy was not acknowledged in time, and a
// replica set whose members retransmit is a replica set whose heartbeats, oplog fetches and client
// replies all arrive late — none of which mongod itself counts.
func tcpRetransmitPanel() Panel {
	return Panel{Name: "TCP retransmissions", Lines: []Line{
		{Label: "retransmitted per segment sent", Path: netstat + "Tcp:RetransSegs", Per: netstat + "Tcp:OutSegs"},
		{Label: "timeouts", Path: netstat + "TcpExt:TCPTimeouts"},
	}}
}

// tcpConnectionsPanel is the machine's view of its connections: how many are open, how many were
// torn down, and how many never made it past a full accept queue.
func tcpConnectionsPanel() Panel {
	return Panel{Name: "TCP connections", Lines: []Line{
		{Label: "established", Path: netstat + "Tcp:CurrEstab"},
		{Label: "resets", Path: netstat + "Tcp:EstabResets"},
		{Label: "resets sent", Path: netstat + "Tcp:OutRsts"},
		{Label: "listen overflows", Path: netstat + "TcpExt:ListenOverflows"},
		{Label: "listen drops", Path: netstat + "TcpExt:ListenDrops"},
	}}
}

func diskTimePanel() Panel {
	return Panel{
		Name:       "Disk busy (ms per second)",
		Match:      func(path string) bool { return diskMetric(path, "io_time_ms") },
		MatchLabel: deviceOf,
	}
}

func diskLatencyPanel() Panel {
	return Panel{
		Name: "Disk read and write time (ms per second)",
		Match: func(path string) bool {
			return diskMetric(path, "read_time_ms") || diskMetric(path, "write_time_ms")
		},
		MatchLabel: func(path string) string {
			kind := "read"
			if strings.HasSuffix(path, "write_time_ms") {
				kind = "write"
			}
			return deviceOf(path) + " " + kind
		},
	}
}

// diskMetric reports whether a path is that counter for a real block device. A floppy or a ram disk
// is never the answer to why a database was slow, and a legend of eight of them hides the one that
// is.
func diskMetric(path, suffix string) bool {
	return strings.HasPrefix(path, disks) && strings.HasSuffix(path, "/"+suffix) &&
		!virtualDevice(deviceOf(path))
}

// virtualDevice reports whether a block device is one no database ever waits on. A legend of eight
// loopbacks hides the disk that is actually the answer.
func virtualDevice(name string) bool {
	for _, prefix := range []string{"fd", "loop", "ram", "sr", "dm-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func deviceOf(path string) string {
	rest := strings.TrimPrefix(path, disks)
	if cut := strings.Index(rest, "/"); cut >= 0 {
		return rest[:cut]
	}
	return rest
}

// UnitOf says what a metric's numbers are, so whoever prints them can use the units a DBA reads
// everywhere else.
//
// Most byte metrics say so in their own path, which is the only rule that scales: MongoDB reports
// hundreds of them and they are not going to be listed here one by one. The exceptions are the
// handful that carry their scale in a suffix or not at all.
func UnitOf(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, "_kb"):
		// /proc/meminfo says kB and means KiB, and FTDC copies the name across.
		return series.UnitKiB
	case strings.HasPrefix(path, "serverStatus/mem/"):
		// serverStatus.mem is mebibytes: resident, virtual, mapped.
		return series.UnitMiB
	case strings.Contains(lower, "bytes"):
		return series.UnitBytes
	case path == tcm+"generic/heap_size":
		// The one tcmalloc figure whose name does not say bytes, and it is the total of ones that do.
		return series.UnitBytes
	}
	return ""
}

// Resolve narrows a panel to the metrics this bundle carries, expanding whatever Match accepts.
// A panel nothing matched is dropped by the caller rather than drawn empty: an empty chart claims
// the server reported nothing when the truth is that this server does not report that at all.
func (p Panel) Resolve(available []string) []Line {
	carried := make(map[string]bool, len(available))
	for _, path := range available {
		carried[path] = true
	}
	lines := make([]Line, 0, len(p.Lines)+4)
	for _, line := range p.Lines {
		path, found := line.Path, carried[line.Path]
		for _, alt := range line.Alt {
			if found {
				break
			}
			path, found = alt, carried[alt]
		}
		if !found {
			continue
		}
		line.Path, line.Alt = path, nil
		if !carriedAll(carried, line.Of) {
			// A share against a total the bundle only half carries is not a smaller answer, it is
			// a wrong one: every percentage would be inflated by whatever is missing.
			continue
		}
		if line.Per != "" && !carried[line.Per] {
			// The divisor is missing, so the ratio cannot be honest. The counter on its own is a
			// different number with a different meaning, and is not a substitute for it.
			continue
		}
		lines = append(lines, line)
	}
	if p.Match != nil {
		for _, path := range available {
			if !p.Match(path) {
				continue
			}
			label := path
			if p.MatchLabel != nil {
				label = p.MatchLabel(path)
			}
			lines = append(lines, Line{Label: label, Path: path})
		}
	}
	return lines
}

// Charted reports whether a path is worth decoding: one the panels draw, or one of the curated set.
// The decoder skips everything else before it is materialised.
func Charted(path string) bool { return Curated(path) || panelMember(path) }

// panelMember is every path the catalogue names or matches.
func panelMember(path string) bool {
	if panelPaths[path] {
		return true
	}
	for _, match := range panelMatchers {
		if match(path) {
			return true
		}
	}
	return false
}

// The index is built once at startup rather than on first use. The decoder asks Charted from
// several goroutines at once — one per file it is inflating — and a map filled in lazily is a map
// two of them write at the same time.
var panelPaths, panelMatchers = indexPanels()

func indexPanels() (map[string]bool, []func(string) bool) {
	paths := map[string]bool{}
	matchers := make([]func(string) bool, 0, 4)
	for _, board := range Boards() {
		for _, panel := range board.Panels {
			if panel.Match != nil {
				matchers = append(matchers, panel.Match)
			}
			for _, line := range panel.Lines {
				paths[line.Path] = true
				if line.Per != "" {
					paths[line.Per] = true
				}
				for _, part := range line.Of {
					paths[part] = true
				}
			}
		}
	}
	return paths, matchers
}

// RealDevice reports whether a block device is one a database could be waiting on: not a loopback,
// a floppy, a ram disk, an optical drive or a device-mapper alias of a disk counted already.
func RealDevice(name string) bool { return !virtualDevice(name) }
