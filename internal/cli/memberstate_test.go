package cli

import (
	"testing"
	"time"

	"github.com/ynsuy/mongo-parser/internal/label"
)

// A node is compared as its role and the one state it held; holding two makes it nobody's peer.
func TestPeerOf(t *testing.T) {
	at := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	span := func(state string, minute int) memberSpan {
		return memberSpan{State: state, From: at.Add(time.Duration(minute) * time.Minute), To: at.Add(time.Duration(minute+1) * time.Minute)}
	}
	for _, c := range []struct {
		role   string
		member replMember
		known  bool
		want   string
	}{
		{"mongod (shard)", replMember{Spans: []memberSpan{span("SECONDARY", 0)}}, true, "mongod (shard) SECONDARY"},
		{"", replMember{Spans: []memberSpan{span("PRIMARY", 0), span("PRIMARY", 5)}}, true, "PRIMARY"},
		{"mongod", replMember{Spans: []memberSpan{span("PRIMARY", 0), span("SECONDARY", 5)}}, true, label.PeerChanged},
		{"mongos", replMember{}, false, "mongos"},
	} {
		if got := peerOf(c.role, c.member, c.known); got != c.want {
			t.Errorf("peerOf(%q, %+v) = %q, want %q", c.role, c.member, got, c.want)
		}
	}
}
