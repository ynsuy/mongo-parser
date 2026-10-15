package metrics

import "strings"

// executionQueue is the concurrency-ticket subtree as 7.0 and later spell it. The counters lived
// under serverStatus.wiredTiger.concurrentTransactions up to 6.x and moved to
// serverStatus.queues.execution in 7.0; 8.0 dropped the old spelling entirely. panels.go already
// listed both names, but a panel only ever sees the curated set, so a list that knows one
// spelling tells every 7.0+ reader their server does not report tickets — which is false, and is
// the one number that says whether the storage engine is the bottleneck.
const executionQueue = "serverStatus/queues/execution/"

// ticketReading says whether a path is one of the ticket figures read per second — the pool and
// what is waiting on it — as opposed to the per-priority counters underneath them, which are
// running totals and belong to the same subtree.
func ticketReading(path string) bool {
	if !strings.HasPrefix(path, executionQueue) {
		return false
	}
	rest := strings.TrimPrefix(path, executionQueue)
	side, field, ok := strings.Cut(rest, "/")
	if !ok || (side != "read" && side != "write") {
		return false
	}
	switch field {
	case "out", "available", "totalTickets", "queueLength":
		return true
	// 8.0 split the queue by admission priority and stopped reporting a queue length beside the
	// pool, so "how many operations were waiting for a ticket" only exists under normalPriority.
	// That is the bucket ordinary work is admitted in; the exempt bucket never waits.
	case "normalPriority/queueLength":
		return true
	}
	return false
}

// Mode says whether a metric is a level or a counter. Exact paths and prefixes only:
// a monotonic-looking gauge is not a counter, and guessing wrong turns a chart into fiction.
func Mode(path string) string {
	if strings.HasPrefix(path, "serverStatus/opcounters/") || strings.HasPrefix(path, "serverStatus/opcountersRepl/") {
		return "rate"
	}
	switch path {
	case "serverStatus/network/bytesIn", "serverStatus/network/bytesOut", "serverStatus/network/numRequests",
		"serverStatus/connections/totalCreated", "serverStatus/connections/rejected", "serverStatus/flowControl/timeAcquiringMicros":
		return "rate"
	// isLaggedCount and isLaggedTimeMicros only ever go up; targetRateLimit and sustainerRate are
	// levels, and reading targetRateLimit as a rate would turn the one number that says writes are
	// being throttled into a meaningless derivative.
	case "serverStatus/flowControl/isLaggedCount", "serverStatus/flowControl/isLaggedTimeMicros":
		return "rate"
	case "serverStatus/flowControl/targetRateLimit", "serverStatus/flowControl/sustainerRate",
		"serverStatus/flowControl/locksPerKiloOp", "serverStatus/flowControl/isLagged",
		"serverStatus/connections/current", "serverStatus/connections/available", "serverStatus/connections/active",
		"serverStatus/mem/resident", "serverStatus/mem/virtual",
		"serverStatus/wiredTiger/cache/bytes currently in the cache", "serverStatus/wiredTiger/cache/maximum bytes configured":
		return "gauge"
	}
	if strings.HasPrefix(path, "serverStatus/wiredTiger/concurrentTransactions/") {
		for _, ending := range []string{"/available", "/out", "/totalTickets"} {
			if strings.HasSuffix(path, ending) {
				return "gauge"
			}
		}
	}
	// The same readings under the name 7.0 moved them to. Everything else in that subtree — the
	// per-priority admissions, queue totals and microsecond sums — only ever goes up.
	if ticketReading(path) {
		return "gauge"
	}
	// The per-priority block beneath them mixes the two kinds under names that look alike:
	// "processing" is how many are running right now, "startedProcessing" is how many ever did.
	// Declaring them is cheaper than a chart that reads one as the other.
	if strings.HasPrefix(path, executionQueue) {
		switch path[strings.LastIndex(path, "/")+1:] {
		case "processing", "queueLength":
			return "gauge"
		case "addedToQueue", "removedFromQueue", "canceled", "newAdmissions",
			"startedProcessing", "finishedProcessing",
			"totalTimeQueuedMicros", "totalTimeProcessingMicros":
			return "rate"
		}
	}
	if strings.HasPrefix(path, "serverStatus/globalLock/currentQueue/") {
		return "gauge"
	}
	// tcmalloc reports its heap two ways in the one subtree, and reading either as the other turns
	// the chart into fiction. The byte figures are levels — how much is in the heap right now — and
	// the counts and the running totals are counters. Declaring them matters more here than
	// elsewhere: half these paths are named alike and only the suffix says which kind it is.
	// The three subtrees the machine itself reports. Every one of them mixes cumulative counters
	// with instantaneous levels, and left undeclared each metric's kind is guessed from whatever
	// the window happened to hold: a counter that did not move inside the window looks flat, is
	// read as a level, and prints its total as though it were a reading. systemMetrics/cpu/nice_ms
	// is the example: its running total drawn as a level, beside user_ms from the same subtree drawn
	// correctly as a rate.
	//
	// /proc/stat: the per-state millisecond figures, the context-switch count and the fork count
	// all accumulate from boot. The process counts are what is running right now, and the core
	// counts and boot time do not change at all.
	if strings.HasPrefix(path, "systemMetrics/cpu/") {
		switch name := path[strings.LastIndex(path, "/")+1:]; name {
		case "ctxt", "processes":
			return "rate"
		case "procs_running", "procs_blocked", "num_logical_cores",
			"num_cores_available_to_process", "btime":
			return "gauge"
		default:
			if strings.HasSuffix(name, "_ms") {
				return "rate"
			}
			return "unknown"
		}
	}
	// /proc/diskstats accumulates everything except how many requests are in flight.
	if strings.HasPrefix(path, "systemMetrics/disks/") {
		if strings.HasSuffix(path, "/io_in_progress") {
			return "gauge"
		}
		return "rate"
	}
	// /proc/net/snmp and /proc/net/netstat count from boot, all but the handful that describe the
	// stack's settings or how many connections are open right now.
	if strings.HasPrefix(path, netstat) {
		switch path[len(netstat):] {
		case "Tcp:CurrEstab", "Tcp:MaxConn", "Tcp:RtoAlgorithm", "Tcp:RtoMin", "Tcp:RtoMax",
			"Ip:Forwarding", "Ip:DefaultTTL":
			return "gauge"
		}
		return "rate"
	}
	// /proc/meminfo is levels throughout: how much memory is in each state at this instant.
	if strings.HasPrefix(path, "systemMetrics/memory/") {
		return "gauge"
	}
	if strings.HasPrefix(path, "serverStatus/tcmalloc/") {
		for _, ending := range []string{"_count", "_total_commit_bytes", "_total_decommit_bytes",
			"_total_reserve_bytes", "_total_delay_ns"} {
			if strings.HasSuffix(path, ending) {
				return "rate"
			}
		}
		return "gauge"
	}
	return "unknown"
}
