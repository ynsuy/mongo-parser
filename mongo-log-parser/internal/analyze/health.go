package analyze

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// A log says most of what it knows in lines nobody reads: a thousand authentication failures, a
// connection churning ten thousand times an hour, a WriteConflict storm under a hot document. None
// of them raises a severity above I, so a screen that ranks by severity never shows them, and a
// grouper that ranks by count buries them under whatever the chattiest component is.
//
// Health names the conditions worth a sentence and counts them by name, so a report can say "this
// happened, this often, starting here" about things the server considered routine.

// Signal is one named condition, counted over the window.
type Signal struct {
	Key         string         `json:"key"`
	Title       string         `json:"title"`
	Severity    string         `json:"severity"` // critical, warning or note
	Count       int            `json:"count"`
	First, Last time.Time      `json:"-"`
	Sources     map[string]int `json:"sources"`
	Example     string         `json:"example,omitempty"`
	Next        string         `json:"next,omitempty"`
}

// Rate is how often this happened per minute over the span it was seen.
func (s Signal) Rate() float64 {
	minutes := s.Last.Sub(s.First).Minutes()
	if minutes < 1 {
		return float64(s.Count)
	}
	return float64(s.Count) / minutes
}

// rule recognises one condition. The match runs against the lowercased message only: MongoDB's
// message field is a fixed string per log statement, so the set of distinct messages in a bundle is
// a few hundred and the decision can be cached per message rather than made per record.
type rule struct {
	key      string
	title    string
	severity string
	next     string
	any      []string // the message matches if it contains any of these
	not      []string // ...and none of these
	all      []string // ...and all of these
}

// rules are ordered: the first that matches names the line. A more specific condition therefore
// comes before the general one it would otherwise be swallowed by.
var rules = []rule{
	{key: "assertion", title: "process-level assertion or fatal signal", severity: "critical",
		next: "logs --category other",
		any:  []string{"fassert", "invariant failure", "terminate() called", "got signal", "aborting after", "unhandled exception"}},
	{key: "disk", title: "storage refused a write", severity: "critical", next: "logs --category storage",
		any: []string{"no space left", "disk full", "quota exceeded", "input/output error", "read-only file system"}},
	{key: "oom", title: "allocation failed or the kernel reclaimed memory", severity: "critical",
		next: "metrics --metric 'system/memory/*'",
		any:  []string{"out of memory", "cannot allocate memory", "bad_alloc", "oom"}},
	{key: "too_stale", title: "a member fell off the sync source's oplog", severity: "critical",
		next: "logs --category replication",
		any:  []string{"too stale", "rollback too large", "we are too far behind"}},
	{key: "rollback_files", title: "rollback wrote documents to a file instead of applying them", severity: "warning",
		next: "logs --category recovery", any: []string{"rollback file", "writing rollback file"}},
	{key: "heartbeat", title: "heartbeats to a member failed", severity: "warning",
		next: "logs --category replication",
		all:  []string{"heartbeat"}, any: []string{"fail", "error", "timed out", "timeout", "retries"}},
	{key: "member_unreachable", title: "a member was reported unreachable", severity: "warning",
		next: "logs --category replication",
		any:  []string{"is now in state rs_down", "is now in state rs_unknown", "no primary detected", "could not find host matching read preference"}},
	{key: "auth_failure", title: "authentication was refused", severity: "warning",
		next: "logs --category authentication",
		any:  []string{"authentication failed", "failed to authenticate", "auth: something", "supported sasl mechanisms requested for unknown user"}},
	{key: "unauthorized", title: "a command was refused for lack of privilege", severity: "note",
		next: "logs --category authentication", any: []string{"not authorized on", "unauthorized"}},
	{key: "write_conflict", title: "writes collided and were retried", severity: "warning",
		next: "slow", any: []string{"writeconflict", "write conflict"}},
	{key: "transaction_abort", title: "transactions were aborted", severity: "note",
		next: "logs --category other", all: []string{"transaction"}, any: []string{"abort"}},
	{key: "lock_timeout", title: "an operation gave up waiting for a lock", severity: "warning",
		next: "metrics --metric 'globalLock/*'",
		any:  []string{"lock acquisition", "lockbusy", "locktimeout", "lock timeout"}},
	// An operation the client gave a maxTimeMS and that ran past it. It is not a lock: the time may
	// have gone anywhere, and the slow query line says where. "Client's executor exceeded time
	// limit" is a different message altogether — the server noting a few microseconds of overrun
	// in its own scheduling — and is not a client's operation at all.
	{key: "max_time", title: "operations ran past their maxTimeMS", severity: "warning",
		next: "slow --failed",
		any:  []string{"maxtimemsexpired", "operation exceeded time limit"},
		not:  []string{"executor exceeded time limit"}},
	{key: "cursor_timeout", title: "cursors were reaped before the client came back", severity: "note",
		next: "logs --category query", all: []string{"cursor"}, any: []string{"timed out", "timeout", "killing"}},
	{key: "killed_op", title: "operations were interrupted or killed", severity: "note",
		next: "logs --category query",
		any:  []string{"interrupted operation", "operation was interrupted", "killop", "operation killed"}},
	{key: "network_error", title: "a connection to another host failed", severity: "warning",
		next: "logs --category network",
		any:  []string{"connection refused", "hostunreachable", "host unreachable", "socket exception", "connection reset by peer", "network error while", "networkinterfaceexceededtimelimit", "connection pool"}},
	{key: "stale_config", title: "a router or shard held a stale routing table", severity: "warning",
		next: "logs --category sharding",
		any:  []string{"staleconfig", "stale config", "stalechunkhistory", "stale shard version", "chunk metadata refresh"}},
	{key: "jumbo_chunk", title: "a chunk was too large to move", severity: "warning",
		next: "logs --category sharding", any: []string{"jumbo", "chunk too big"}},
	{key: "balancer_off", title: "the balancer was stopped or could not run", severity: "note",
		next: "logs --category sharding",
		all:  []string{"balancer"}, any: []string{"stopped", "disabled", "off", "failed"}},
	{key: "slow_storage", title: "the storage engine reported an operation taking too long", severity: "warning",
		next: "metrics --metric 'wiredTiger/*'",
		any:  []string{"took too long", "slow operation", "checkpoint took", "unable to evict", "cache eviction", "waiting for cache"},
		not:  []string{"slow query"}},
	{key: "clock", title: "the system clock moved or disagreed with a peer", severity: "warning",
		next: "logs --category other", any: []string{"clock skew", "time jump", "system time", "clock drift"}},
	{key: "connection_accepted", title: "connections were opened", severity: "note",
		next: "metrics --metric 'connections/*'", any: []string{"connection accepted"}},
	{key: "connection_ended", title: "connections were closed", severity: "note",
		next: "metrics --metric 'connections/*'", any: []string{"connection ended"}},
}

// errorAttributes is the part of a log line's attributes that carries the reason something failed.
// MongoDB writes a generic message and puts the interesting half in here — "Error receiving request
// from client" says nothing, and its error attribute says HostUnreachable.
type errorAttributes struct {
	Error  json.RawMessage `json:"error"`
	Reason json.RawMessage `json:"reason"`
	Status json.RawMessage `json:"status"`
}

// Health counts the named conditions of a log stream.
type Health struct {
	signals map[string]*Signal
	decided map[string]int // message text -> index into rules, or -1 for no rule
}

func NewHealth() *Health {
	return &Health{signals: map[string]*Signal{}, decided: map[string]int{}}
}

// Add counts one record against the condition its message names, if any.
func (h *Health) Add(record Record) {
	index, known := h.decided[record.Msg]
	if !known {
		index = matchRule(strings.ToLower(record.Msg))
		h.decided[record.Msg] = index
	}
	if index < 0 {
		// The message said nothing the rules recognise. On a line the server itself called a
		// problem, the reason is in the attributes rather than in the message, and reading it is
		// affordable precisely because those lines are the rare ones.
		switch record.Sev {
		case "F", "E", "W":
			index = matchRule(errorText(record))
		default:
			return
		}
	}
	if index < 0 {
		// Still nothing named, but the server called it fatal, and nothing else in this tool turns
		// an F into a named condition.
		if record.Sev != "F" {
			return
		}
		h.count(Signal{Key: "fatal", Title: "the server logged a fatal line", Severity: "critical",
			Next: "logs --category " + string(Classify(record))}, record)
		return
	}
	matched := rules[index]
	h.count(Signal{Key: matched.key, Title: matched.title, Severity: matched.severity, Next: matched.next}, record)
}

func (h *Health) count(template Signal, record Record) {
	signal := h.signals[template.Key]
	if signal == nil {
		template.First, template.Last = record.At, record.At
		template.Sources = map[string]int{}
		template.Example = record.Msg
		signal = &template
		h.signals[template.Key] = signal
	}
	signal.Count++
	signal.Sources[record.Source]++
	if record.At.Before(signal.First) {
		signal.First = record.At
	}
	if record.At.After(signal.Last) {
		signal.Last = record.At
	}
}

// errorText is the lowercased reason a line gives for failing, or "" when it gives none.
func errorText(record Record) string {
	var attributes errorAttributes
	if !record.DecodeAttributes(&attributes) {
		return ""
	}
	parts := make([]string, 0, 3)
	for _, raw := range [][]byte{attributes.Error, attributes.Reason, attributes.Status} {
		if len(raw) > 0 {
			parts = append(parts, string(raw))
		}
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func matchRule(message string) int {
	if message == "" {
		return -1
	}
	for index, candidate := range rules {
		if !containsAll(message, candidate.all) || !containsAny(message, candidate.any) || containsOne(message, candidate.not) {
			continue
		}
		return index
	}
	return -1
}

func containsAll(message string, needles []string) bool {
	for _, needle := range needles {
		if !strings.Contains(message, needle) {
			return false
		}
	}
	return true
}

// containsAny is true for an empty list: a rule with no "any" clause is constrained only by "all".
func containsAny(message string, needles []string) bool {
	return len(needles) == 0 || containsOne(message, needles)
}

// containsOne is false for an empty list, which is what an absent "not" clause has to mean.
func containsOne(message string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}

// severityOrder ranks the three words a signal can carry.
var severityOrder = map[string]int{"critical": 0, "warning": 1, "note": 2}

// Signals returns what was seen, worst first.
func (h *Health) Signals() []Signal {
	out := make([]Signal, 0, len(h.signals))
	for _, signal := range h.signals {
		out = append(out, *signal)
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := severityOrder[out[i].Severity], severityOrder[out[j].Severity]
		if left != right {
			return left < right
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Get returns one signal by key, and whether it was seen at all.
func (h *Health) Get(key string) (Signal, bool) {
	signal, seen := h.signals[key]
	if !seen {
		return Signal{}, false
	}
	return *signal, true
}

// ConnectionChurn is how often a connection was opened over the window. A pool that reconnects
// instead of holding its connections shows up here and almost nowhere else: the connection count
// looks steady, and every one of those handshakes costs authentication on the server.
func (h *Health) ConnectionChurn() (opened, closed int, perMinute float64) {
	accepted, hasAccepted := h.Get("connection_accepted")
	ended, _ := h.Get("connection_ended")
	if !hasAccepted {
		return 0, ended.Count, 0
	}
	return accepted.Count, ended.Count, accepted.Rate()
}
