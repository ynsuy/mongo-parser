package analyze

import (
	"testing"
	"time"
)

// moment is a point inside the window every test in this file uses.
func moment(hour, minute int) time.Time {
	return time.Date(2026, 9, 8, hour, minute, 0, 0, time.UTC)
}

var windowStart, windowEnd = moment(0, 0), moment(12, 0)

// transition is a line a member writes about itself.
func transition(hour, minute int, from, to string) string {
	return `{"t":{"$date":"` + moment(hour, minute).Format(time.RFC3339) +
		`"},"s":"I","c":"REPL","id":21358,"ctx":"ReplCoord-0","msg":"Replica set state transition","attr":{"newState":"` +
		to + `","oldState":"` + from + `"}}` + "\n"
}

// heartbeat is a line a member writes about somebody else.
func heartbeat(hour, minute int, member, state string) string {
	return `{"t":{"$date":"` + moment(hour, minute).Format(time.RFC3339) +
		`"},"s":"I","c":"REPL","id":21215,"ctx":"ReplCoord-2","msg":"Member is in new state","attr":{"hostAndPort":"` +
		member + `","newState":"` + state + `"}}` + "\n"
}

// down is the other heartbeat line, the one that spells the state in the message.
func down(hour, minute int, member string) string {
	return `{"t":{"$date":"` + moment(hour, minute).Format(time.RFC3339) +
		`"},"s":"I","c":"REPL","id":21216,"ctx":"ReplCoord-2","msg":"Member is now in state RS_DOWN","attr":{"hostAndPort":"` +
		member + `","heartbeatMessage":"Error connecting"}}` + "\n"
}

// config is the pair of lines a member writes when it loads a replica set config: the roster, and
// which of its entries it is.
func config(hour, minute int, set string, members []string, self string) string {
	hosts := ""
	for i, member := range members {
		if i > 0 {
			hosts += ","
		}
		hosts += `{"host":"` + member + `"}`
	}
	return `{"t":{"$date":"` + moment(hour, minute).Format(time.RFC3339) +
		`"},"s":"I","c":"REPL","id":21392,"ctx":"ReplCoord-0","msg":"New replica set config in use","attr":{"config":{"_id":"` +
		set + `","members":[` + hosts + `]}}}` + "\n" +
		`{"t":{"$date":"` + moment(hour, minute).Format(time.RFC3339) +
		`"},"s":"I","c":"REPL","id":21393,"ctx":"ReplCoord-0","msg":"Found self in config","attr":{"hostAndPort":"` +
		self + `"}}` + "\n"
}

func readMembers(t *testing.T, nodes ...nodeLog) []MemberHistory {
	t.Helper()
	return readNodes(t, nodes...).Members(windowStart, windowEnd)
}

func find(t *testing.T, members []MemberHistory, name string) MemberHistory {
	t.Helper()
	for _, member := range members {
		if member.Name() == name || member.Source == name {
			return member
		}
	}
	t.Fatalf("no member called %q among %d", name, len(members))
	return MemberHistory{}
}

// stateAt is what the history says, as a pair a test can read in one line.
func stateAt(member MemberHistory, hour, minute int) (string, Evidence) {
	span := member.StateAt(moment(hour, minute))
	return span.State, span.Evidence
}

// TestUnknownBeforeAndAfterEveryLog is the rule the rest of this file rests on: outside the reach
// of every log that ever spoke about a member, its state is unknown rather than whatever was last
// true. Carrying the last state forward is how a set that had a primary throughout was reported as
// having none.
func TestUnknownBeforeAndAfterEveryLog(t *testing.T) {
	members := readMembers(t, nodeLog{"a",
		config(3, 0, "rs0", []string{"a:27017"}, "a:27017") +
			transition(3, 0, "STARTUP2", "SECONDARY") +
			transition(4, 0, "SECONDARY", "PRIMARY")})

	member := find(t, members, "a:27017")
	if state, evidence := stateAt(member, 1, 0); evidence != Unknown || state != "" {
		t.Errorf("before the log started: %q/%s, want unknown", state, evidence)
	}
	if state, evidence := stateAt(member, 3, 30); state != "SECONDARY" || evidence != Observed {
		t.Errorf("inside the log: %q/%s, want SECONDARY/observed", state, evidence)
	}
	// The log's last line is at 04:00, so everything after it is outside what anything was watching.
	if state, evidence := stateAt(member, 9, 0); evidence != Unknown || state != "" {
		t.Errorf("after the log ended: %q/%s, want unknown", state, evidence)
	}
}

// TestPeerFillsInAfterAMemberSLogEnds is the case the whole design exists for: logs in one bundle
// stop at different hours, and past the end of a member's own log its peers are the only thing that
// knows where it got to.
func TestPeerFillsInAfterAMemberSLogEnds(t *testing.T) {
	members := readMembers(t,
		nodeLog{"a",
			config(1, 0, "rs0", []string{"a:27017", "b:27017"}, "a:27017") +
				transition(1, 0, "STARTUP2", "SECONDARY")},
		nodeLog{"b",
			config(1, 0, "rs0", []string{"a:27017", "b:27017"}, "b:27017") +
				transition(1, 0, "STARTUP2", "SECONDARY") +
				heartbeat(6, 0, "a:27017", "PRIMARY") +
				transition(11, 0, "SECONDARY", "SECONDARY")})

	a := find(t, members, "a:27017")
	// Its own log stops at 01:00 and says SECONDARY; its peer watched it become PRIMARY at 06:00.
	if state, evidence := stateAt(a, 3, 0); state != "SECONDARY" || evidence != Observed {
		t.Errorf("inside its own log: %q/%s", state, evidence)
	}
	if state, evidence := stateAt(a, 8, 0); state != "PRIMARY" || evidence != Reported {
		t.Errorf("after its own log ended: %q/%s, want PRIMARY/reported", state, evidence)
	}
	// And past the peer's log too, nothing knows.
	if _, evidence := stateAt(a, 11, 30); evidence != Unknown {
		t.Errorf("past every log: %s, want unknown", evidence)
	}
}

// TestPeerCannotOverruleAMemberSOwnLog covers what a heartbeat actually means. "Member is now in
// state RS_DOWN" says the peer could not reach it — a partition, a firewall, a node too busy to
// answer — and the member may have been serving the whole time. Read as the member's own state it
// wipes hours off a timeline whose members were logging queries throughout.
func TestPeerCannotOverruleAMemberSOwnLog(t *testing.T) {
	members := readMembers(t,
		nodeLog{"a",
			config(1, 0, "rs0", []string{"a:27017", "b:27017"}, "a:27017") +
				transition(1, 0, "SECONDARY", "PRIMARY") +
				// Something on this node at 10:00, so its log demonstrably covers the whole stretch.
				transition(10, 0, "PRIMARY", "PRIMARY")},
		nodeLog{"b",
			config(1, 0, "rs0", []string{"a:27017", "b:27017"}, "b:27017") +
				transition(1, 0, "STARTUP2", "SECONDARY") +
				down(5, 0, "a:27017")})

	a := find(t, members, "a:27017")
	if state, evidence := stateAt(a, 6, 0); state != "PRIMARY" || evidence != Observed {
		t.Errorf("a peer's unreachability became the member's state: %q/%s", state, evidence)
	}
}

// TestMemberWithNoLogOfItsOwn: a config names members this bundle may hold no log for, and leaving
// them out is how a missing shard goes unnoticed in a report about the set that has one.
func TestMemberWithNoLogOfItsOwn(t *testing.T) {
	members := readMembers(t, nodeLog{"a",
		config(1, 0, "rs0", []string{"a:27017", "b:27017", "c:27017"}, "a:27017") +
			transition(1, 0, "STARTUP2", "SECONDARY") +
			heartbeat(2, 0, "b:27017", "PRIMARY") +
			transition(9, 0, "SECONDARY", "SECONDARY")})

	if len(members) != 3 {
		t.Fatalf("read %d members, want the three the config names", len(members))
	}
	b := find(t, members, "b:27017")
	if b.HasLog() {
		t.Error("b has no log in this bundle and should not claim one")
	}
	if state, evidence := stateAt(b, 5, 0); state != "PRIMARY" || evidence != Reported {
		t.Errorf("b at 05:00 = %q/%s, want PRIMARY/reported", state, evidence)
	}
	// c is in the config and nobody ever said anything about it. That is not an error and it is
	// not a state either.
	c := find(t, members, "c:27017")
	if _, evidence := stateAt(c, 5, 0); evidence != Unknown {
		t.Errorf("c was never mentioned and reads as %s", evidence)
	}
}

// TestWritableSaysWhenASetCouldTakeAWrite is the derived row, and the only thing in the report that
// answers the question a replication problem is actually about.
func TestWritableSaysWhenASetCouldTakeAWrite(t *testing.T) {
	cluster := readNodes(t,
		nodeLog{"a",
			config(1, 0, "rs0", []string{"a:27017", "b:27017"}, "a:27017") +
				transition(1, 0, "STARTUP2", "SECONDARY") +
				transition(2, 0, "SECONDARY", "PRIMARY") +
				transition(4, 0, "PRIMARY", "SECONDARY") +
				transition(10, 0, "SECONDARY", "PRIMARY")},
		nodeLog{"b",
			config(1, 0, "rs0", []string{"a:27017", "b:27017"}, "b:27017") +
				transition(1, 0, "STARTUP2", "SECONDARY") +
				transition(10, 30, "SECONDARY", "SECONDARY")})

	members := cluster.Members(windowStart, windowEnd)
	summaries := cluster.Summaries(windowStart, windowEnd, members)
	if len(summaries) != 1 {
		t.Fatalf("read %d sets, want rs0 alone: %+v", len(summaries), summaries)
	}
	summary := summaries[0]
	// No primary from 04:00 to 10:00.
	if summary.LongestGap != 6*time.Hour {
		t.Errorf("longest stretch with no primary = %s, want 6h", summary.LongestGap)
	}
	if !summary.GapStarted.Equal(moment(4, 0)) {
		t.Errorf("the gap started at %s, want 04:00", summary.GapStarted.Format("15:04"))
	}
	if summary.LastPrimary != "a:27017" || !summary.LastPrimaryAt.Equal(moment(10, 0)) {
		t.Errorf("last primary = %s at %s", summary.LastPrimary, summary.LastPrimaryAt.Format("15:04"))
	}
	if summary.Members != 2 || summary.WithLogs != 2 {
		t.Errorf("members = %d of which %d have logs", summary.Members, summary.WithLogs)
	}
	// Nothing was running before 01:00 and everything had stopped by 10:30, so two and a half
	// hours of the window are not something the set can be said to have spent either way.
	if summary.Unwatched != 150*time.Minute {
		t.Errorf("unwatched = %s, want 2h30m", summary.Unwatched)
	}
}

// TestSetThatNeverHadAPrimary is a different report from one whose primary went away, and the two
// used to be the same sentence.
func TestSetThatNeverHadAPrimary(t *testing.T) {
	cluster := readNodes(t, nodeLog{"a",
		config(1, 0, "rs0", []string{"a:27017"}, "a:27017") +
			transition(1, 0, "STARTUP2", "RECOVERING") +
			transition(8, 0, "RECOVERING", "RECOVERING")})

	members := cluster.Members(windowStart, windowEnd)
	summary := cluster.Summaries(windowStart, windowEnd, members)[0]
	if summary.LastPrimary != "" {
		t.Errorf("claimed a primary of %q where there was never one", summary.LastPrimary)
	}
	if summary.LongestGap != 7*time.Hour {
		t.Errorf("longest stretch with no primary = %s, want the 7h anything was watching", summary.LongestGap)
	}
}

// TestUnjoinedLogStillGetsAHistory: a log that never wrote "Found self in config" — rotated past
// its own startup, most often — cannot be tied to the name its peers use, and its history is still
// worth drawing under the only name it has.
func TestUnjoinedLogStillGetsAHistory(t *testing.T) {
	members := readMembers(t, nodeLog{"lonely",
		transition(2, 0, "STARTUP2", "SECONDARY") +
			transition(5, 0, "SECONDARY", "PRIMARY")})

	if len(members) != 1 {
		t.Fatalf("read %d members, want the one log: %+v", len(members), members)
	}
	member := members[0]
	if member.HostAndPort != "" || member.Source == "" {
		t.Errorf("an unjoined log should be named by its file: %+v", member)
	}
	if state, evidence := stateAt(member, 3, 0); state != "SECONDARY" || evidence != Observed {
		t.Errorf("state at 03:00 = %q/%s, want SECONDARY/observed", state, evidence)
	}
	// The log ends on the line that records the change, so PRIMARY is the last thing known about
	// this member: the span has no width, and it has to survive anyway or the member reads as never
	// having been in the state it ended in.
	found := false
	for _, span := range member.Spans {
		if span.State == "PRIMARY" && span.From.Equal(moment(5, 0)) {
			found = true
		}
	}
	if !found {
		t.Errorf("the state the log ended on was dropped: %+v", member.Spans)
	}
	if _, evidence := stateAt(member, 6, 0); evidence != Unknown {
		t.Errorf("an hour after the log ended reads as %s", evidence)
	}
}

// TestFlapBeyondWhatIsKeptSaysSo. A member that changes state thousands of times has a history this
// package stops recording, and the first hundred of them drawn to the end of the window is a
// flapping member drawn as a steady one.
func TestFlapBeyondWhatIsKeptSaysSo(t *testing.T) {
	text := config(0, 1, "rs0", []string{"a:27017"}, "a:27017")
	for i := range maxTransitions + 20 {
		state := "SECONDARY"
		if i%2 == 0 {
			state = "RECOVERING"
		}
		text += transition(1, i, "PRIMARY", state)
	}

	member := find(t, readMembers(t, nodeLog{"a", text}), "a:27017")
	if !member.Partial {
		t.Error("a history that ran past what is kept must say it is partial")
	}
}

// stop and start are the lines a process writes on its way out and on its way back.
func stop(hour, minute int) string {
	return `{"t":{"$date":"` + moment(hour, minute).Format(time.RFC3339) +
		`"},"s":"I","c":"CONTROL","id":23138,"ctx":"SignalHandler","msg":"Shutting down","attr":{"exitCode":0}}` + "\n"
}

func start(hour, minute int) string {
	return `{"t":{"$date":"` + moment(hour, minute).Format(time.RFC3339) +
		`"},"s":"I","c":"CONTROL","id":4615611,"ctx":"initandlisten","msg":"MongoDB starting","attr":{"pid":9,"port":27017}}` + "\n"
}

// TestAStoppedSetIsNotASetWithoutAPrimary is the difference between a maintenance window and an
// outage, and every count in a log says the same thing about both.
//
// When every member of a set exits cleanly and comes back hours later, no member is PRIMARY because
// no member is running. Reported as a stretch with no primary it reads as the worst incident in the
// bundle, when it was planned work.
func TestAStoppedSetIsNotASetWithoutAPrimary(t *testing.T) {
	var nodes []nodeLog
	for _, name := range []string{"a", "b"} {
		nodes = append(nodes, nodeLog{name,
			config(1, 0, "rs0", []string{"a:27017", "b:27017"}, name+":27017") +
				transition(1, 0, "STARTUP2", "SECONDARY") +
				// a is the primary until everything stops at 04:00 and comes back at 09:00.
				map[string]string{"a": transition(1, 1, "SECONDARY", "PRIMARY"), "b": ""}[name] +
				stop(4, 0) + start(9, 0) +
				transition(9, 1, "STARTUP2", "SECONDARY") +
				map[string]string{"a": transition(9, 2, "SECONDARY", "PRIMARY"), "b": ""}[name] +
				transition(11, 0, "SECONDARY", "SECONDARY")})
	}

	cluster := readNodes(t, nodes...)
	members := cluster.Members(windowStart, windowEnd)
	a := find(t, members, "a:27017")
	if state, evidence := stateAt(a, 6, 0); state != StateStopped || evidence != Observed {
		t.Errorf("a at 06:00 = %q/%s, want STOPPED/observed", state, evidence)
	}
	summary := cluster.Summaries(windowStart, windowEnd, members)[0]
	if summary.LongestStop != 5*time.Hour {
		t.Errorf("shut down for %s, want 5h", summary.LongestStop)
	}
	// The only stretch that counts as "no primary" is the couple of minutes between coming back up
	// and electing one, not the five hours nothing was running.
	if summary.LongestGap > 3*time.Minute {
		t.Errorf("counted %s as a set with no primary; the set was switched off", summary.LongestGap)
	}
}

// TestAPeerLosingAStoppedMemberIsNotNews: every peer notices a clean exit a second or two after it
// happens, and those notices land just past the end of the member's own log. Left in, the last
// thing a timeline said about a node that was switched off deliberately was that it was unreachable.
func TestAPeerLosingAStoppedMemberIsNotNews(t *testing.T) {
	members := readMembers(t,
		nodeLog{"a",
			config(1, 0, "rs0", []string{"a:27017", "b:27017"}, "a:27017") +
				transition(1, 0, "STARTUP2", "SECONDARY") +
				stop(4, 0)},
		nodeLog{"b",
			config(1, 0, "rs0", []string{"a:27017", "b:27017"}, "b:27017") +
				transition(1, 0, "STARTUP2", "PRIMARY") +
				down(4, 1, "a:27017") +
				transition(10, 0, "PRIMARY", "PRIMARY")})

	a := find(t, members, "a:27017")
	if state, evidence := stateAt(a, 6, 0); state != StateStopped || evidence != Observed {
		t.Errorf("a at 06:00 = %q/%s, want STOPPED/observed: its own log said it exited", state, evidence)
	}
}
