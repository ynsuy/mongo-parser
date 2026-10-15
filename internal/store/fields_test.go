package store

import (
	"encoding/json"
	"testing"
)

// The scanner must pick what encoding/json picks, or answer that it cannot.
func TestFieldAgreesWithEncodingJSON(t *testing.T) {
	lines := []string{
		`{"t":{"$date":"2026-10-12T14:32:47.307+03:00"},"s":"W","c":"NETWORK","id":1,"ctx":"conn1","msg":"x","attr":{"error":"boom","n":1}}`,
		`{"t":{"$date":"x"},"s":"I","msg":"quote \"attr\": {\"fake\":1} inside","attr":{"real":true}}`,
		`{"msg":"m","attr":null}`,
		`{"msg":"m"}`,
		`{"msg":"m","Attr":{"a":1},"attr":{"b":2}}`,
		`{"msg":"m","attr":{"a":1},"ATTR":[1,2,{"c":"}"}]}`,
		`{ "msg" : "spaced" , "attr" : { "k" : "v" } , "truncated" : {"attr":{"x":1}} }`,
		`{"msg":"esc \\","attr":{"s":"\u00e7\"]}"}}`,
		`{"msg":"m","attr":{"a":[1,{"b":[]}],"c":-1.5e3,"d":false}}`,
		`{}`,
	}
	for _, line := range lines {
		var want struct {
			Attr json.RawMessage `json:"attr"`
		}
		if err := json.Unmarshal([]byte(line), &want); err != nil {
			t.Fatalf("bad test line %s: %v", line, err)
		}
		got, found, ok := Field(line, "attr")
		if !ok {
			t.Errorf("%s: scanner gave up", line)
			continue
		}
		if string(want.Attr) != got || found != (want.Attr != nil) {
			t.Errorf("%s: got %q found=%v, encoding/json %q", line, got, found, want.Attr)
		}
		r := Record{Raw: line}
		if string(r.Attr()) != string(want.Attr) {
			t.Errorf("%s: Attr() %q, want %q", line, r.Attr(), want.Attr)
		}
	}
	for _, broken := range []string{`{"attr":{"a":1}`, `{"attr" {"a":1}}`, `["attr"]`, `{"attr":"unterminated}`} {
		if _, _, ok := Field(broken, "attr"); ok {
			t.Errorf("%s: scanner accepted it", broken)
		}
	}
}
