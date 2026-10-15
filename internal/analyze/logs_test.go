package analyze

import "testing"

const lines = `{"t":{"$date":"2026-09-03T10:00:00.100-06:00"},"s":"I","c":"NETWORK","id":22943,"ctx":"listener","msg":"Connection accepted","attr":{"remote":"10.0.0.1:5000","connectionId":41}}
{"t":{"$date":"2026-09-03T10:00:01.000-06:00"},"s":"I","c":"NETWORK","id":22943,"ctx":"listener","msg":"Connection accepted","attr":{"remote":"10.0.0.2:5001","connectionId":42}}
{"t":{"$date":"2026-09-03T10:05:00.000-06:00"},"s":"W","c":"REPL","id":21432,"ctx":"conn9","msg":"Election failed after heartbeat timeout","attr":{"term":7}}
not a log line at all
{"t":{"$date":"2026-09-03T10:06:00.000-06:00"},"s":"I","c":"COMMAND","id":51803,"ctx":"conn9","msg":"Slow query","attr":{"durationMillis":900,"planSummary":"COLLSCAN"}}
`

// A rotation is part of the same log, and it is usually the compressed part.
const rotated = `{"t":{"$date":"2026-09-02T09:00:00.000-06:00"},"s":"E","c":"STORAGE","id":22435,"ctx":"conn2","msg":"WiredTiger error"}
{"t":{"$date":"2026-09-02T09:00:05.000-06:00"},"s":"I","c":"NETWORK","id":22943,"ctx":"listener","msg":"Connection accepted","attr":{"remote":"10.0.0.9:5009"}}
`

// Lines that say the same thing are one row however many nodes said it, and the rows are ranked so
// the one worth reading first is first.
func TestGroupsFoldWhatSaysTheSameThing(t *testing.T) {
	grouper := NewGrouper()
	feed(t, "node-a", lines, grouper.Only(""))
	feed(t, "node-b", rotated, grouper.Only(""))

	groups := grouper.Groups()
	byMessage := map[string]Group{}
	for _, group := range groups {
		byMessage[group.Message] = group
	}
	// The three accepted connections differ only in address and id, so they are one group.
	if connections := byMessage["Connection accepted"]; connections.Count != 3 {
		t.Errorf("connection group counted %d, want 3", connections.Count)
	}
	if len(byMessage["Connection accepted"].Sources) != 2 {
		t.Error("a group spanning two nodes should name both sources")
	}
	// An error outranks a warning, which outranks the rest.
	if groups[0].Message != "WiredTiger error" {
		t.Errorf("first group is %q, want the error", groups[0].Message)
	}
	if got := byMessage["Slow query"].Category; got != SlowQuery {
		t.Errorf("slow query classified as %q", got)
	}
	if got := byMessage["Election failed after heartbeat timeout"].Category; got != Election {
		t.Errorf("election classified as %q", got)
	}
}

// Ids, addresses and long numbers vary line by line; folding them is what makes groups useful.
func TestFingerprint(t *testing.T) {
	for message, want := range map[string]string{
		"Connection ended for 10.0.0.1:5000":        "Connection ended for <value>",
		"Index build 507f1f77bcf86cd799439011 done": "Index build <value> done",
		"Slow query took 12345 ms":                  "Slow query took <value> ms",
		"Election failed":                           "Election failed",
	} {
		if got := Fingerprint(message); got != want {
			t.Errorf("Fingerprint(%q) = %q, want %q", message, got, want)
		}
	}
}

func TestAttributesAreDecodedOnlyWhenAsked(t *testing.T) {
	var slow Record
	feed(t, "node", lines, sinkFunc(func(r Record) {
		if r.Msg == "Slow query" {
			slow = r
		}
	}))

	var attributes struct {
		DurationMillis int    `json:"durationMillis"`
		PlanSummary    string `json:"planSummary"`
	}
	if !slow.DecodeAttributes(&attributes) {
		t.Fatal("slow query attributes did not decode")
	}
	if attributes.DurationMillis != 900 || attributes.PlanSummary != "COLLSCAN" {
		t.Errorf("decoded %+v", attributes)
	}
	if got := slow.Attributes()["planSummary"]; got != "COLLSCAN" {
		t.Errorf("map access returned %v", got)
	}
}

type sinkFunc func(Record)

func (f sinkFunc) Add(record Record) { f(record) }
