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
	Key           string         `json:"key"`
	Title         string         `json:"title"`
	Priority      string         `json:"review_priority"` // high, medium or low; not server severity or impact
	LogSeverities map[string]int `json:"log_severities"`
	MatchBasis    map[string]int `json:"match_basis"`
	MatchAny      []string       `json:"match_any,omitempty"`
	MatchAll      []string       `json:"match_all,omitempty"`
	MatchNot      []string       `json:"match_not,omitempty"`
	Count         int            `json:"count"`
	First         time.Time      `json:"first_at,omitzero"`
	Last          time.Time      `json:"last_at,omitzero"`
	Sources       map[string]int `json:"sources"`
	// Examples holds the first matched line for each match basis, with the pattern that matched and
	// the text it matched in, so a match on a line's error attributes is not shown beside a message
	// that never says what was matched.
	Examples []MatchExample `json:"examples,omitempty"`
	Next     string         `json:"next,omitempty"`
}

// MatchExample is one line a signal matched, and what in it matched.
type MatchExample struct {
	Basis   string    `json:"basis"`
	At      time.Time `json:"at"`
	Source  string    `json:"source"`
	Message string    `json:"message"`
	// Matched is the configured pattern found in the line; empty when the basis is the logged
	// severity alone.
	Matched string `json:"matched,omitempty"`
	// MatchedIn is the lowercased attribute text the pattern was found in, when it was not the
	// message, cut to exampleTextLimit bytes.
	MatchedIn string `json:"matched_in,omitempty"`
}

// exampleTextLimit bounds the attribute text kept with an example.
const exampleTextLimit = 300

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
	{key: "assertion", title: "assertion, signal or termination text matched", severity: "high",
		next: "logs --category other",
		any:  []string{"fassert", "invariant failure", "terminate() called", "got signal", "aborting after", "unhandled exception"}},
	{key: "disk", title: "storage error text matched", severity: "high", next: "logs --category storage",
		any: []string{"no space left", "disk full", "quota exceeded", "input/output error", "read-only file system"}},
	{key: "oom", title: "allocation or out-of-memory text matched", severity: "high",
		next: "metrics --metric 'system/memory/*'",
		any:  []string{"out of memory", "cannot allocate memory", "bad_alloc", "oom"}},
	{key: "too_stale", title: "staleness or rollback-size text matched", severity: "high",
		next: "logs --category replication",
		any:  []string{"too stale", "rollback too large", "we are too far behind"}},
	{key: "rollback_files", title: "rollback-file text matched", severity: "medium",
		next: "logs --category recovery", any: []string{"rollback file", "writing rollback file"}},
	{key: "heartbeat", title: "heartbeat failure or retry text matched", severity: "medium",
		next: "logs --category replication",
		all:  []string{"heartbeat"}, any: []string{"fail", "error", "timed out", "timeout", "retries"}},
	{key: "member_unreachable", title: "member reachability or primary-selection text matched", severity: "medium",
		next: "logs --category replication",
		any:  []string{"is now in state rs_down", "is now in state rs_unknown", "no primary detected", "could not find host matching read preference"}},
	{key: "auth_failure", title: "authentication failure or unknown-user text matched", severity: "medium",
		next: "logs --category authentication",
		any:  []string{"authentication failed", "failed to authenticate", "auth: something", "supported sasl mechanisms requested for unknown user"}},
	{key: "unauthorized", title: "authorization refusal text matched", severity: "low",
		next: "logs --category authentication", any: []string{"not authorized on", "unauthorized"}},
	{key: "write_conflict", title: "write-conflict text matched", severity: "medium",
		next: "slow", any: []string{"writeconflict", "write conflict"}},
	{key: "transaction_abort", title: "transaction abort text matched", severity: "low",
		next: "logs --category other", all: []string{"transaction"}, any: []string{"abort"}},
	{key: "lock_timeout", title: "lock-acquisition or lock-error text matched", severity: "medium",
		next: "metrics --metric 'globalLock/*'",
		any:  []string{"lock acquisition", "lockbusy", "locktimeout", "lock timeout"}},
	// An operation the client gave a maxTimeMS and that ran past it. It is not a lock: the time may
	// have gone anywhere, and the slow query line says where. "Client's executor exceeded time
	// limit" is a different message altogether — the server noting a few microseconds of overrun
	// in its own scheduling — and is not a client's operation at all.
	{key: "max_time", title: "operation time-limit text matched", severity: "medium",
		next: "slow --failed",
		any:  []string{"maxtimemsexpired", "operation exceeded time limit"},
		not:  []string{"executor exceeded time limit"}},
	{key: "cursor_timeout", title: "cursor timeout or kill text matched", severity: "low",
		next: "logs --category query", all: []string{"cursor"}, any: []string{"timed out", "timeout", "killing"}},
	{key: "killed_op", title: "operation interruption or kill text matched", severity: "low",
		next: "logs --category query",
		any:  []string{"interrupted operation", "operation was interrupted", "killop", "operation killed"}},
	{key: "network_error", title: "network, reachability or connection-pool text matched", severity: "medium",
		next: "logs --category network",
		any:  []string{"connection refused", "hostunreachable", "host unreachable", "socket exception", "connection reset by peer", "network error while", "networkinterfaceexceededtimelimit", "connection pool"}},
	{key: "stale_config", title: "routing metadata or refresh text matched", severity: "medium",
		next: "logs --category sharding",
		any:  []string{"staleconfig", "stale config", "stalechunkhistory", "stale shard version", "chunk metadata refresh"}},
	{key: "jumbo_chunk", title: "jumbo or chunk-size text matched", severity: "medium",
		next: "logs --category sharding", any: []string{"jumbo", "chunk too big"}},
	{key: "balancer_off", title: "balancer state or failure text matched", severity: "low",
		next: "logs --category sharding",
		all:  []string{"balancer"}, any: []string{"stopped", "disabled", "off", "failed"}},
	{key: "slow_storage", title: "storage timing, eviction or cache-wait text matched", severity: "medium",
		next: "metrics --metric 'wiredTiger/*'",
		any:  []string{"took too long", "slow operation", "checkpoint took", "unable to evict", "cache eviction", "waiting for cache"},
		not:  []string{"slow query"}},
	{key: "clock", title: "system-time or clock text matched", severity: "medium",
		next: "logs --category other", any: []string{"clock skew", "time jump", "system time", "clock drift"}},
	{key: "connection_accepted", title: "connection-accepted text matched", severity: "low",
		next: "metrics --metric 'connections/*'", any: []string{"connection accepted"}},
	{key: "connection_ended", title: "connection-ended text matched", severity: "low",
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
	basis, text := "message_text", ""
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
			text = errorText(record)
			index = matchRule(text)
			basis = "error_attributes"
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
		h.count(Signal{Key: "fatal", Title: "the server logged a fatal line", Priority: "high",
			Next: "logs --category " + string(Classify(record))}, record, "logged_severity_F", func() MatchExample {
			return MatchExample{}
		})
		return
	}
	matched := rules[index]
	h.count(Signal{Key: matched.key, Title: matched.title, Priority: matched.severity, Next: matched.next,
		MatchAny: matched.any, MatchAll: matched.all, MatchNot: matched.not}, record, basis, func() MatchExample {
		if basis == "message_text" {
			return MatchExample{Matched: matched.found(strings.ToLower(record.Msg))}
		}
		in := text
		if len(in) > exampleTextLimit {
			in = strings.ToValidUTF8(in[:exampleTextLimit], "")
		}
		return MatchExample{Matched: matched.found(text), MatchedIn: in}
	})
}

// found is the pattern of the rule that the text contains: the first "any" pattern, or the first
// "all" pattern for a rule constrained by "all" alone.
func (r rule) found(text string) string {
	for _, needles := range [][]string{r.any, r.all} {
		for _, needle := range needles {
			if strings.Contains(text, needle) {
				return needle
			}
		}
	}
	return ""
}

// count adds one record to its signal. describe says what matched; it is only called for the first
// line of each basis, which is all an example needs.
func (h *Health) count(template Signal, record Record, basis string, describe func() MatchExample) {
	signal := h.signals[template.Key]
	if signal == nil {
		template.First, template.Last = record.At, record.At
		template.Sources = map[string]int{}
		template.LogSeverities = map[string]int{}
		template.MatchBasis = map[string]int{}
		signal = &template
		h.signals[template.Key] = signal
	}
	if signal.MatchBasis[basis] == 0 {
		example := describe()
		example.Basis, example.At, example.Source, example.Message = basis, record.At, record.Source, record.Msg
		signal.Examples = append(signal.Examples, example)
	}
	signal.Count++
	signal.LogSeverities[record.Sev]++
	signal.MatchBasis[basis]++
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

// severityOrder ranks fixed review priorities, not MongoDB log severity or incident impact.
var severityOrder = map[string]int{"high": 0, "medium": 1, "low": 2}

// Signals returns what was seen, worst first.
func (h *Health) Signals() []Signal {
	out := make([]Signal, 0, len(h.signals))
	for _, signal := range h.signals {
		out = append(out, *signal)
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := severityOrder[out[i].Priority], severityOrder[out[j].Priority]
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
