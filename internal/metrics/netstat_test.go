package metrics

import (
	"testing"
)

// The retransmission panel draws the share, which needs the segments sent beside it.
func TestTCPPanelDrawsTheShare(t *testing.T) {
	lines := tcpRetransmitPanel().Resolve([]string{netstat + "Tcp:RetransSegs", netstat + "Tcp:OutSegs", netstat + "TcpExt:TCPTimeouts"})
	if len(lines) != 2 || lines[0].Per != netstat+"Tcp:OutSegs" {
		t.Fatalf("lines = %+v, want the share first and the timeouts after it", lines)
	}
}
