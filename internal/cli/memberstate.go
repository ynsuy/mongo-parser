package cli

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ynsuy/mongo-parser/internal/analyze"
	"github.com/ynsuy/mongo-parser/internal/ftdc"
	"github.com/ynsuy/mongo-parser/internal/label"
	"github.com/ynsuy/mongo-parser/internal/render"
)

// Which member was taking the writes is the first question of most replica set cases, and the logs
// very often cannot answer it: a node states its role when it transitions, so a log rotated past
// the last election says nothing, and an export filtered to one severity drops the lines entirely.
//
// Every node's own serverStatus carries the two flags, and FTDC records them with every sample. The
// server renamed `ismaster` to `isWritablePrimary` in 5.0 and writes both for a while, so both are
// read and the newer one wins.
const (
	primaryMetric   = "serverStatus/repl/isWritablePrimary"
	masterMetric    = "serverStatus/repl/ismaster"
	secondaryMetric = "serverStatus/repl/secondary"
)

// neitherState is a member that is neither: starting up, recovering, rolling back, or an arbiter.
// serverStatus has only the two flags, so which of those it was is not in this data.
const neitherState = "neither"

// memberSpan is a stretch during which a node reported one state.
type memberSpan struct {
	State string    `json:"state"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
}

// replMember is one node's run of them, in order.
type replMember struct {
	Source string       `json:"source"`
	Spans  []memberSpan `json:"spans"`
}

// EndedAs is the state the node was last recorded in.
func (m replMember) EndedAs() string { return m.Spans[len(m.Spans)-1].State }

// replMemberMetric reports whether a metric path is one of the flags, for folding this into a read
// that is happening anyway: decompressing a bundle's diagnostic data twice to ask two questions of
// it is most of the cost of the report.
func replMemberMetric(path string) bool {
	return path == primaryMetric || path == masterMetric || path == secondaryMetric
}

// replMemberOf reads the flags out of samples already decoded for something else.
func replMemberOf(samples *ftdc.Samples, source string) (replMember, bool) {
	primary := samples.Values[primaryMetric]
	if len(primary) == 0 {
		primary = samples.Values[masterMetric]
	}
	secondary := samples.Values[secondaryMetric]
	if len(primary) == 0 && len(secondary) == 0 {
		return replMember{}, false
	}
	read := func(series []float64, i int) (float64, bool) {
		if i >= len(series) || math.IsNaN(series[i]) {
			return 0, false
		}
		return series[i], true
	}
	var spans []memberSpan
	for i, millis := range samples.Timestamps {
		isPrimary, knownPrimary := read(primary, i)
		isSecondary, knownSecondary := read(secondary, i)
		if !knownPrimary && !knownSecondary {
			continue // the sample carried neither flag: unknown, which is not a state
		}
		state := neitherState
		switch {
		case knownPrimary && isPrimary != 0:
			state = "PRIMARY"
		case knownSecondary && isSecondary != 0:
			state = "SECONDARY"
		}
		when := time.UnixMilli(millis).UTC()
		if n := len(spans); n > 0 && spans[n-1].State == state {
			spans[n-1].To = when
			continue
		}
		spans = append(spans, memberSpan{State: state, From: when, To: when})
	}
	if len(spans) == 0 {
		return replMember{}, false
	}
	return replMember{Source: source, Spans: spans}, true
}

// peerOf is what a node is compared to other nodes as: its role and the replica set state it held
// through the window, or label.PeerChanged when it held more than one. A node with no state flags
// (a standalone, a mongos) is its role alone.
func peerOf(role string, member replMember, known bool) string {
	if !known {
		return role
	}
	for _, span := range member.Spans[1:] {
		if span.State != member.Spans[0].State {
			return label.PeerChanged
		}
	}
	return strings.TrimSpace(role + " " + member.Spans[0].State)
}

// sortMemberStates puts the primary first: it is what the question is usually about.
func sortMemberStates(states []replMember) {
	sort.SliceStable(states, func(i, j int) bool {
		left, right := states[i].EndedAs() == "PRIMARY", states[j].EndedAs() == "PRIMARY"
		if left != right {
			return left
		}
		return states[i].Source < states[j].Source
	})
}

// renderMemberStates is the table of them, with the note that says where it came from. An empty
// list renders as nothing: a bundle without diagnostic data has no answer here, and a heading over
// nothing reads as one.
func renderMemberStates(states []replMember, options render.TableOptions) string {
	if len(states) == 0 {
		return ""
	}
	// A line per node rather than a table cell: the history is the answer, and a cell cut it off.
	lines := []string{"member state, from FTDC (serverStatus.repl): what each node said it was, every sample"}
	neither := false
	for _, member := range states {
		neither = neither || member.EndedAs() == neitherState
		wrapped := render.Wrap(fmt.Sprintf("%s ended as %s: %s", member.Source, member.EndedAs(), describeSpans(member.Spans)), options.Width-2, 4)
		wrapped[0] = "  " + wrapped[0]
		lines = append(lines, wrapped...)
	}
	if neither {
		lines = append(lines, "\""+neitherState+"\" is neither primary nor secondary — starting up, recovering, rolling back, or an arbiter")
	}
	return strings.Join(lines, "\n")
}

func describeSpans(spans []memberSpan) string {
	if len(spans) == 1 {
		return fmt.Sprintf("%s throughout (%s → %s)", spans[0].State, shortMoment(spans[0].From), shortMoment(spans[0].To))
	}
	// A transition is where this gets read, so each one is printed as the moment it happened. The
	// first span starts where the data does rather than where anything changed, and says so.
	parts := make([]string, 0, len(spans))
	parts = append(parts, fmt.Sprintf("%s from %s", spans[0].State, shortMoment(spans[0].From)))
	for _, span := range spans[1:] {
		parts = append(parts, fmt.Sprintf("%s at %s", span.State, shortMoment(span.From)))
	}
	const shown = 4
	if len(parts) > shown {
		rest := len(parts) - shown
		parts = append(parts[:shown], fmt.Sprintf("and %d more changes", rest))
	}
	return strings.Join(parts, ", ")
}

func shortMoment(at time.Time) string { return at.UTC().Format("01-02 15:04") }

// nameMembers names the member behind each per-member label by its position in the node's logged
// config, the order replSetGetStatus lists members in.
func nameMembers(results []label.Result, cluster *analyze.Cluster) {
	for i := range results {
		r := &results[i]
		if r.Instance == "" || !strings.HasPrefix(r.Rule().Signal.Num[0], "replSetGetStatus/members/") {
			continue
		}
		members := cluster.ConfigMembers(r.Source)
		if n, err := strconv.Atoi(r.Instance); err == nil && n >= 0 && n < len(members) {
			r.Member = members[n].String()
		}
	}
}
