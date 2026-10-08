package analyze

import "testing"

func readHealth(t *testing.T, text string) *Health {
	t.Helper()
	health := NewHealth()
	feed(t, "node-a", text, health)
	return health
}

// These are the lines a deployment writes in the thousands at severity I, so nothing that ranks by
// severity or by count ever shows them.
const healthLines = `{"t":{"$date":"2026-09-03T10:00:00Z"},"s":"I","c":"ACCESS","id":20249,"ctx":"conn1","msg":"Authentication failed","attr":{"mechanism":"SCRAM-SHA-256","principalName":"app","error":"AuthenticationFailed"}}
{"t":{"$date":"2026-09-03T10:01:00Z"},"s":"I","c":"ACCESS","id":20249,"ctx":"conn2","msg":"Authentication failed","attr":{"mechanism":"SCRAM-SHA-256","principalName":"app","error":"AuthenticationFailed"}}
{"t":{"$date":"2026-09-03T10:02:00Z"},"s":"I","c":"WRITE","id":51803,"ctx":"conn3","msg":"WriteConflict caught during write, retrying","attr":{"ns":"app.orders"}}
{"t":{"$date":"2026-09-03T10:03:00Z"},"s":"I","c":"NETWORK","id":22943,"ctx":"listener","msg":"Connection accepted","attr":{"remote":"10.0.0.1:5000","connectionCount":40}}
{"t":{"$date":"2026-09-03T10:03:01Z"},"s":"I","c":"NETWORK","id":22944,"ctx":"conn4","msg":"Connection ended","attr":{"remote":"10.0.0.1:5000","connectionCount":39}}
`

func TestHealthNamesTheConditionsNothingElseRanks(t *testing.T) {
	health := readHealth(t, healthLines)
	auth, seen := health.Get("auth_failure")
	if !seen || auth.Count != 2 {
		t.Errorf("auth_failure = %+v, want 2", auth)
	}
	if auth.Severity != "warning" {
		t.Errorf("auth_failure severity = %q, want warning", auth.Severity)
	}
	if conflict, seen := health.Get("write_conflict"); !seen || conflict.Count != 1 {
		t.Errorf("write_conflict = %+v, want 1", conflict)
	}
	if opened, closed, _ := health.ConnectionChurn(); opened != 1 || closed != 1 {
		t.Errorf("churn = %d opened, %d closed; want 1 and 1", opened, closed)
	}
}

// MongoDB writes a generic message and puts the reason in the attributes: the message says "Error
// receiving request from client", and only the error attribute says the host was unreachable.
func TestHealthReadsTheReasonOutOfTheAttributes(t *testing.T) {
	health := readHealth(t,
		`{"t":{"$date":"2026-09-03T10:00:00Z"},"s":"E","c":"NETWORK","id":22988,"ctx":"conn1","msg":"Error receiving request from client. Ending connection from remote","attr":{"error":"HostUnreachable: Connection reset by peer","remote":"10.0.0.9:4321"}}`+"\n")

	if signal, seen := health.Get("network_error"); !seen || signal.Count != 1 {
		t.Errorf("network_error = %+v, want 1", signal)
	}
}

// An ordinary line at severity I whose message the rules do not name must not have its attributes
// decoded at all, or a screen of named conditions costs a full parse of every log line there is.
func TestHealthIgnoresOrdinaryTraffic(t *testing.T) {
	health := readHealth(t,
		`{"t":{"$date":"2026-09-03T10:00:00Z"},"s":"I","c":"REPL","id":21331,"ctx":"conn1","msg":"Logging transition to primary to oplog on stepup","attr":{"error":"HostUnreachable"}}`+"\n")

	if signals := health.Signals(); len(signals) != 0 {
		t.Errorf("signals = %+v, want none: an I-severity line's attributes are not read", signals)
	}
}

func TestHealthCountsAFatalLineTheRulesCannotName(t *testing.T) {
	health := readHealth(t,
		`{"t":{"$date":"2026-09-03T10:00:00Z"},"s":"F","c":"CONTROL","id":99999,"ctx":"conn1","msg":"Something the rules have never heard of"}`+"\n")

	signal, seen := health.Get("fatal")
	if !seen || signal.Count != 1 || signal.Severity != "critical" {
		t.Errorf("fatal = %+v, want one critical", signal)
	}
}
