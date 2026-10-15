package analyze

import (
	"sort"
	"strings"
	"time"
)

// This file answers one question the per-node counters cannot: what state was each member of each
// replica set in, at each moment of the window, and when did the set have nobody to write to.
//
// The counters say a node held elections and ended SECONDARY. They cannot say whether the set
// had a primary the whole time, because "ended" is a different moment for every node — logs in one
// bundle rarely stop together — and because a node's own log is not the only thing that knows where
// it got to. Three kinds of evidence exist, and a member's history is the three of them merged:
//
//   - the member's own "Replica set state transition" lines, which are as direct as it gets;
//   - its peers' "Member is in new state" and "Member is now in state X" heartbeat lines, which are
//     the only evidence there is once its own log has ended, and the only evidence at all for a
//     member whose log is not in the bundle;
//   - the stretch of time each log covers, which is what separates "the member stayed SECONDARY"
//     from "nothing was watching".

// Evidence says where a statement about a member's state came from. A reader deciding whether to
// act on a timeline needs to know which of the two it is: a member's own log is a fact about the
// member, and a peer's heartbeat is a fact about what that peer could see.
type Evidence string

const (
	// Observed is the member's own log saying what it became.
	Observed Evidence = "observed"
	// Reported is a peer's heartbeat saying what it saw the member become.
	Reported Evidence = "reported"
	// Unknown is no log covering the member at that moment at all.
	Unknown Evidence = "unknown"
)

// StateSpan is one stretch of the window a member spent in one state.
//
// An unknown span carries no state: it is the honest answer where a log ended, a log had not
// started, or a member has no log in the bundle and nobody was watching it. Filling it in with the
// last state seen is what reported a set as having no writable node when it had one all along.
type StateSpan struct {
	From     time.Time `json:"from,omitzero"`
	To       time.Time `json:"to,omitzero"`
	State    string    `json:"state,omitempty"`
	Evidence Evidence  `json:"evidence"`
	// Changes is how many state changes happened inside this span's bin once it has been binned for
	// drawing. It is zero on the spans this package builds and set by whoever bins them.
	Changes int `json:"changes,omitempty"`
}

// Known reports whether anything was watching this member over this span.
func (s StateSpan) Known() bool { return s.Evidence != Unknown }

// MemberHistory is everything the bundle knows about one replica set member.
type MemberHistory struct {
	// Set is the replica set this member belongs to, where a log said so.
	Set string `json:"replica_set,omitempty"`
	// HostAndPort is the member's name in the replica set config, which is the name its peers use
	// for it. Empty for a log that never logged the config it found itself in.
	HostAndPort string `json:"host_and_port,omitempty"`
	// Source is the log file that is this member, empty when the bundle has no log for it. A
	// member with no source is one this bundle only ever heard about second hand.
	Source string      `json:"source,omitempty"`
	Spans  []StateSpan `json:"spans"`
	// Partial is set when more state changes happened than were kept. A member that flapped a
	// thousand times has a history this package deliberately stops recording, and a timeline drawn
	// from the first hundred of them would show a steady member. Saying the history is partial is
	// the only honest way to draw what is left of it.
	Partial bool `json:"history_partial,omitempty"`
}

// HasLog reports whether the bundle holds this member's own log.
func (m MemberHistory) HasLog() bool { return m.Source != "" }

// Name is what to call this member: the name its set knows it by, or failing that its log.
func (m MemberHistory) Name() string {
	if m.HostAndPort != "" {
		return m.HostAndPort
	}
	return m.Source
}

// StateAt is the state this member was in at one moment, and how that is known.
func (m MemberHistory) StateAt(moment time.Time) StateSpan {
	for _, span := range m.Spans {
		if !moment.Before(span.From) && moment.Before(span.To) {
			return span
		}
	}
	// Only then the spans with no width: a state entered on the last line a log ever wrote covers
	// the one instant it has, and nothing after it. Taking it first would let an instant of
	// knowledge stand for every hour that followed, which is the mistake this whole file is about.
	for _, span := range m.Spans {
		if span.To.Equal(span.From) && moment.Equal(span.From) {
			return span
		}
	}
	return StateSpan{From: moment, To: moment, Evidence: Unknown}
}

// stateEvent is one statement that a member entered a state at a moment.
type stateEvent struct {
	at       time.Time
	state    string
	evidence Evidence
	// seenBy is the log that made the statement, which is what ties a statement to a coverage span.
	seenBy string
}

// coverage is the stretch of time one log file spans. Silence inside it is the absence of a change;
// silence outside it is the absence of a log, and the two must never render alike.
type coverage struct{ first, last time.Time }

// identity ties a log file to the member it is, from the "Found self in config" line the server
// writes whenever it loads a replica set config.
//
// Nothing else in a log reliably gives this. The host a process reports at startup is the machine's
// own name — "host-a.internal" on a cloud host — while its peers know it by whatever the replica
// set config says, "rs0-a.example.com:27017". Matching those two by string finds nothing, and
// without the join a member's own history and its peers' account of it stay two separate rows
// describing one machine.
type identity struct {
	hostAndPort string
	set         string
}

// Members is every replica set member the bundle knows of, with its state history over the window,
// ordered by set and then by name.
//
// The window is the caller's: a history is only as long as the report it belongs to, and the spans
// are clipped to it so a chart drawn from them lines up with every other chart.
func (c *Cluster) Members(from, to time.Time) []MemberHistory {
	if !to.After(from) {
		return nil
	}
	c.confirmClaims()
	events := c.memberEvents()
	// Every member the bundle has heard of, whether or not anything was said about its state: a
	// config names members that may have no log here and may never have been reachable, and leaving
	// them out of the table is how a missing shard goes unnoticed.
	names := map[string]bool{}
	for member := range events {
		names[member] = true
	}
	for member := range c.roster {
		names[member] = true
	}

	histories := make([]MemberHistory, 0, len(names))
	for name := range names {
		history := MemberHistory{HostAndPort: name, Set: c.roster[name], Source: c.sourceOf(name)}
		if history.Set == "" {
			history.Set = c.setOf(name)
		}
		history.Spans = c.spans(events[name], c.coverage[history.Source], c.watchedUntil(history.Set), from, to)
		history.Partial = c.truncated[name] || (history.Source != "" && c.nodes[history.Source] != nil && c.nodes[history.Source].Truncated)
		histories = append(histories, history)
	}
	// A log whose member never logged the config it found itself in cannot be joined to a name, and
	// it still has a history worth drawing. It goes in under the only name it has.
	for source, node := range c.nodes {
		if _, joined := c.selves[source]; joined || len(node.Transitions) == 0 {
			continue
		}
		history := MemberHistory{Source: source, Set: node.ReplicaSet, Partial: node.Truncated}
		history.Spans = c.spans(c.ownEvents(source), c.coverage[source], c.watchedUntil(node.ReplicaSet), from, to)
		histories = append(histories, history)
	}

	sort.Slice(histories, func(i, j int) bool {
		if histories[i].Set != histories[j].Set {
			return histories[i].Set < histories[j].Set
		}
		return histories[i].Name() < histories[j].Name()
	})
	return histories
}

// confirmClaims accepts a log's own idea of which member it is, but only where the set's config
// agrees that such a member exists.
//
// A process states the machine's hostname and its port. Sometimes that is exactly the name the
// replica set knows it by, and on a 7.0 deployment whose logs were rotated past the last restart
// it is the only thing that could be. Sometimes it is nothing like it — a cloud instance calls
// itself host-a.internal while its set knows it as rs0-a.example.com — and taking it would invent
// a member that does not exist and leave the real one with no log.
// Checking it against the roster is what tells the two apart.
func (c *Cluster) confirmClaims() {
	for source, name := range c.claimed {
		if _, joined := c.selves[source]; joined {
			continue
		}
		if set, listed := c.roster[name]; listed {
			c.selves[source] = identity{hostAndPort: name, set: set}
		}
	}
}

// memberEvents gathers every statement about every member, keyed by the member it is about.
func (c *Cluster) memberEvents() map[string][]stateEvent {
	events := map[string][]stateEvent{}
	for source, node := range c.nodes {
		self, joined := c.selves[source]
		if !joined {
			continue
		}
		events[self.hostAndPort] = append(events[self.hostAndPort], c.ownEvents(source)...)
		_ = node
	}
	for member, reports := range c.reports {
		events[member] = append(events[member], reports...)
	}
	return events
}

// ownEvents is what one log said about itself.
func (c *Cluster) ownEvents(source string) []stateEvent {
	node := c.nodes[source]
	if node == nil {
		return nil
	}
	out := make([]stateEvent, 0, len(node.Transitions)+len(c.lifecycle[source]))
	for _, transition := range node.Transitions {
		out = append(out, stateEvent{at: transition.At, state: transition.To, evidence: Observed, seenBy: source})
	}
	// The process starting and stopping is as much a statement about the member's state as any
	// transition: between a clean exit and the next start it was not in a state, it was not there.
	out = append(out, c.lifecycle[source]...)
	return out
}

// sourceOf is the log file that is this member, where the bundle has one. A node's log is rotated
// and a bundle keeps every rotation, so there is usually more than one; the earliest by name is
// taken, because map order is not an order and the same bundle has to read the same way twice.
func (c *Cluster) sourceOf(hostAndPort string) string {
	found := ""
	for source, self := range c.selves {
		if self.hostAndPort != hostAndPort {
			continue
		}
		if found == "" || source < found {
			found = source
		}
	}
	return found
}

// Known reports whether anything at all is known about this member's state over the window.
func (m MemberHistory) Known() bool {
	for _, span := range m.Spans {
		if span.Known() {
			return true
		}
	}
	return false
}

// setOf is the replica set a member belongs to according to whichever log identified itself as it.
func (c *Cluster) setOf(hostAndPort string) string {
	for _, self := range c.selves {
		if self.hostAndPort == hostAndPort {
			return self.set
		}
	}
	return ""
}

// ownFirst drops the peer reports that fall inside the stretch the member's own log covers.
//
// A peer's heartbeat says what that peer could see, which is not the same as what the member was.
// "Member is now in state RS_DOWN" means the peer could not reach it — a partition, a firewall, a
// busy node that missed two heartbeats — and the member may have been serving throughout. Drawn as
// the member's own state it wiped hours off a timeline whose members were logging queries the
// whole time.
//
// So inside its own log's reach the member's own word is the only evidence used, and the peers'
// account is what fills the stretches its own log does not cover. Heartbeats that failed are
// counted and reported as what they are, under the node that could not do the reaching.
func ownFirst(events []stateEvent, own coverage) []stateEvent {
	if own.first.IsZero() && own.last.IsZero() {
		return events
	}
	kept := events[:0:0]
	for _, event := range events {
		if event.evidence == Reported && !event.at.Before(own.first) && !event.at.After(own.last) {
			continue
		}
		kept = append(kept, event)
	}
	return dropDownAfterStop(kept)
}

// dropDownAfterStop removes the peers' "I cannot reach it" from the stretch after a member said it
// was shutting down.
//
// A clean exit is followed within a second or two by every peer noticing the member has gone, and
// those notices land just past the end of the member's own log — so the last thing a timeline said
// about a node that was switched off deliberately was that its peers had lost it. Both are true and
// only one of them is the answer: the member's own log already said it exited.
func dropDownAfterStop(events []stateEvent) []stateEvent {
	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	kept := events[:0:0]
	stopped := false
	for _, event := range events {
		switch {
		case event.evidence == Observed && event.state == StateStopped:
			stopped = true
		case event.evidence == Observed:
			stopped = false
		case stopped && event.state == "DOWN":
			continue
		}
		kept = append(kept, event)
	}
	return kept
}

// spans turns the statements about one member into the stretches of time they describe.
//
// A state holds from the moment it was entered until the next statement — but only while something
// was watching. Past the end of every log that ever said anything about this member, the state is
// not "the last one seen", it is unknown, and the difference is the whole point of the exercise.
// watchedUntil is the last moment any log belonging to a replica set was still running.
//
// A member is watched for as long as somebody in its set is up, not only for as long as somebody
// happened to write a line about it: a peer that is running logs a change the moment it sees one,
// so its silence about a member is evidence that the member did not change. The exposure that buys
// is a partitioned peer, which sees nothing and therefore says nothing — which is why a span
// carries the evidence it rests on and a report says which.
func (c *Cluster) watchedUntil(set string) time.Time {
	watched := time.Time{}
	for source, node := range c.nodes {
		if set != "" && node.ReplicaSet != set && c.selves[source].set != set {
			continue
		}
		if end := c.coverage[source].last; end.After(watched) {
			watched = end
		}
	}
	return watched
}

func (c *Cluster) spans(events []stateEvent, own coverage, setWatched, from, to time.Time) []StateSpan {
	events = ownFirst(events, own)
	if len(events) == 0 {
		return []StateSpan{{From: from, To: to, Evidence: Unknown}}
	}
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].at.Equal(events[j].at) {
			return events[i].at.Before(events[j].at)
		}
		// A member's own word beats a peer's account of it at the same instant.
		if (events[i].evidence == Observed) != (events[j].evidence == Observed) {
			return events[i].evidence == Observed
		}
		// Two peers can report a member differently at the same second — one of them is looking at
		// a view it has not caught up with, and nothing here can say which. Ordering by the log
		// that spoke at least makes the same bundle draw the same way twice.
		if events[i].seenBy != events[j].seenBy {
			return events[i].seenBy < events[j].seenBy
		}
		return events[i].state < events[j].state
	})

	// watched is the last moment any log that ever spoke about this member was still running. It is
	// the union rather than the maximum of one: a peer that is up and watching logs a change when
	// one happens, so its silence is evidence that nothing changed, and its log ending is the
	// moment that evidence stops.
	watched := setWatched
	for _, event := range events {
		if end := c.coverage[event.seenBy].last; end.After(watched) {
			watched = end
		}
	}

	spans := make([]StateSpan, 0, len(events)+2)
	add := func(span StateSpan) {
		if span.From.Before(from) {
			span.From = from
		}
		if span.To.After(to) {
			span.To = to
		}
		if !span.To.After(span.From) {
			return
		}
		spans = append(spans, span)
	}
	if events[0].at.After(from) {
		add(StateSpan{From: from, To: events[0].at, Evidence: Unknown})
	}
	for i, event := range events {
		end := watched
		if i+1 < len(events) {
			end = events[i+1].at
		}
		if end.Before(event.at) {
			end = event.at
		}
		span := StateSpan{From: event.at, To: end, State: event.state, Evidence: event.evidence}
		// A log can end on the very line that records a state change, which leaves the last span
		// with no width at all. Dropped, it says the member was never in that state; the truth is
		// that it entered it and then the log stopped, so the span is kept as the instant it was.
		if i == len(events)-1 && !end.After(event.at) && !event.at.Before(from) && !event.at.After(to) {
			spans = append(spans, span)
			continue
		}
		add(span)
	}
	if watched.Before(to) {
		add(StateSpan{From: watched, To: to, Evidence: Unknown})
	}
	return spans
}

// Writable is, for each replica set, the stretches of the window in which one of its members was in
// PRIMARY — and the stretches in which the evidence was good enough to say that none was.
//
// It is the one line a reader wants before any other: a set with no primary is a set that cannot
// take a write, and no per-node counter says whether that ever happened. A stretch in which nothing
// was watching is neither, and says so.
func (c *Cluster) Writable(from, to time.Time, members []MemberHistory) map[string][]StateSpan {
	bySet := map[string][]MemberHistory{}
	for _, member := range members {
		bySet[member.Set] = append(bySet[member.Set], member)
	}
	out := make(map[string][]StateSpan, len(bySet))
	for set, group := range bySet {
		out[set] = writableSpans(group, from, to)
	}
	return out
}

// writableSpans walks one set's members in step, at every moment any of them changed.
func writableSpans(members []MemberHistory, from, to time.Time) []StateSpan {
	moments := []time.Time{from}
	for _, member := range members {
		for _, span := range member.Spans {
			if span.From.After(from) && span.From.Before(to) {
				moments = append(moments, span.From)
			}
		}
	}
	sort.Slice(moments, func(i, j int) bool { return moments[i].Before(moments[j]) })

	spans := make([]StateSpan, 0, len(moments))
	for i, moment := range moments {
		if i > 0 && moment.Equal(moments[i-1]) {
			continue
		}
		end := to
		for j := i + 1; j < len(moments); j++ {
			if moments[j].After(moment) {
				end = moments[j]
				break
			}
		}
		// "A member was PRIMARY" needs one member to say so. "No member was PRIMARY" needs all of
		// them: a set with one member nothing was watching could have had its primary there, and
		// a report that says otherwise is guessing. So the weakest evidence in the set decides.
		//
		// And a set whose every process had exited is not a set without a primary. Both are an
		// absence of PRIMARY and they are opposite reports — one is an outage, the other is a
		// maintenance window — so they are counted apart.
		state, evidence, running := writableNone, Observed, false
		for _, member := range members {
			at := member.StateAt(moment)
			if at.State == "PRIMARY" {
				state, evidence, running = writablePrimary, at.Evidence, true
				break
			}
			if !at.Known() {
				state, evidence = "", Unknown
				break
			}
			if at.State != StateStopped {
				running = true
			}
			if at.Evidence == Reported {
				evidence = Reported
			}
		}
		if state == writableNone && !running {
			state = writableStopped
		}
		if len(spans) > 0 && spans[len(spans)-1].State == state && spans[len(spans)-1].Evidence == evidence {
			spans[len(spans)-1].To = end
			continue
		}
		spans = append(spans, StateSpan{From: moment, To: end, State: state, Evidence: evidence})
	}
	return spans
}

// The two states a set can be in, as far as taking a write goes.
const (
	writablePrimary = "PRIMARY"
	writableNone    = "NONE"
	// writableStopped is every member of the set shut down. Not writable, and not a failure.
	writableStopped = StateStopped
)

// SetSummary is what a replica set did over the window, in the terms a reader asks about it.
//
// It exists because a chart cannot be read for exact moments. A band drawn at one column per
// quarter hour cannot show the few minutes a member spent in PRIMARY at the end of a window, and
// a reader who needs to know whether the set ever came back has to be told rather than shown.
type SetSummary struct {
	Set string `json:"replica_set"`
	// Members is how many members the set has, as far as any config in the bundle said.
	Members int `json:"members"`
	// WithLogs is how many of them this bundle holds a log for. The rest are second hand.
	WithLogs int `json:"members_with_logs"`

	// LongestGap is the longest stretch in which no member was in PRIMARY and the logs were good
	// enough to say so, and GapStarted is when it began. TotalGap adds every such stretch up.
	LongestGap time.Duration `json:"longest_no_primary"`
	TotalGap   time.Duration `json:"total_no_primary"`
	GapStarted time.Time     `json:"gap_started_at,omitzero"`

	// LastPrimary is the member seen in PRIMARY latest, when, and how that is known. An empty
	// member means no log in this bundle ever saw one.
	LastPrimary   string    `json:"last_primary,omitempty"`
	LastPrimaryAt time.Time `json:"last_primary_at,omitzero"`
	LastPrimaryBy Evidence  `json:"last_primary_evidence,omitempty"`
	// Unwatched is how much of the window nothing was watching the set at all, which is the part
	// none of the above can speak for.
	Unwatched time.Duration `json:"unwatched"`
	// Stopped is how long every member of the set was shut down, and StoppedFrom when the longest
	// such stretch began. A set that is not running has no primary, and saying only that would
	// report a maintenance window as an outage.
	Stopped     time.Duration `json:"stopped"`
	LongestStop time.Duration `json:"longest_stop"`
	StoppedFrom time.Time     `json:"stopped_from,omitzero"`
	// Partial is set when any member of the set changed state more often than was recorded, so
	// everything above is a floor rather than a figure.
	Partial bool `json:"history_partial,omitempty"`
}

// Summaries is one SetSummary per replica set, in the order the sets are drawn.
func (c *Cluster) Summaries(from, to time.Time, members []MemberHistory) []SetSummary {
	writable := c.Writable(from, to, members)
	order := make([]string, 0, len(writable))
	seen := map[string]bool{}
	out := make([]SetSummary, 0, len(writable))
	for _, member := range members {
		if !seen[member.Set] {
			seen[member.Set] = true
			order = append(order, member.Set)
		}
	}
	for _, set := range order {
		summary := SetSummary{Set: set}
		for _, member := range members {
			if member.Set != set {
				continue
			}
			summary.Members++
			if member.HasLog() {
				summary.WithLogs++
			}
			summary.Partial = summary.Partial || member.Partial
			for _, span := range member.Spans {
				if span.State == "PRIMARY" && span.From.After(summary.LastPrimaryAt) {
					summary.LastPrimary, summary.LastPrimaryAt, summary.LastPrimaryBy = member.Name(), span.From, span.Evidence
				}
			}
		}
		summary.LongestGap, summary.TotalGap, summary.GapStarted = NoPrimary(writable[set])
		for _, span := range writable[set] {
			switch {
			case !span.Known():
				summary.Unwatched += span.To.Sub(span.From)
			case span.State == writableStopped:
				length := span.To.Sub(span.From)
				summary.Stopped += length
				if length > summary.LongestStop {
					summary.LongestStop, summary.StoppedFrom = length, span.From
				}
			}
		}
		out = append(out, summary)
	}
	return out
}

// NoPrimary is the longest stretch in which a set demonstrably had no member in PRIMARY, and how
// long the set spent that way in total. A zero duration means it never happened as far as the logs
// can tell, which is not the same as it never having happened.
func NoPrimary(spans []StateSpan) (longest time.Duration, total time.Duration, at time.Time) {
	for _, span := range spans {
		if span.State != writableNone {
			continue
		}
		length := span.To.Sub(span.From)
		total += length
		if length > longest {
			longest, at = length, span.From
		}
	}
	return longest, total, at
}

// reportedState reads the state out of "Member is now in state X", which spells it in the message
// rather than in an attribute — and is the only line that ever says a member went down.
func reportedState(message string) string {
	const prefix = "member is now in state "
	lower := strings.ToLower(message)
	index := strings.Index(lower, prefix)
	if index < 0 {
		return ""
	}
	state := strings.ToUpper(strings.TrimSpace(message[index+len(prefix):]))
	// RS_DOWN is how the heartbeat spells it; the state a reader knows is DOWN.
	state = strings.TrimPrefix(state, "RS_")
	if !memberStates[state] {
		return ""
	}
	return state
}
