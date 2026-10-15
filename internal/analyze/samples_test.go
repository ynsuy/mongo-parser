package analyze

import "testing"

// "Are the writes slow" is the question people have; answering it otherwise means remembering that
// findAndModify is a write and getMore is a read.
func TestMatchesOperation(t *testing.T) {
	for _, testcase := range []struct {
		spec, operation string
		want            bool
	}{
		{"", "delete", true},
		{"writes", "delete", true},
		{"writes", "insert", true},
		{"writes", "findAndModify", true},
		{"writes", "find", false},
		{"writes", "getMore", false},
		{"reads", "aggregate", true},
		{"reads", "getMore", true},
		{"reads", "update", false},
		{"delete", "delete", true},
		{"DELETE", "delete", true},
		{"getmore", "getMore", true},
		{"insert,delete", "delete", true},
		{"insert, delete", "insert", true},
		{"insert,delete", "update", false},
		{"nonsense", "delete", false},
	} {
		if got := MatchesOperation(testcase.spec, testcase.operation); got != testcase.want {
			t.Errorf("MatchesOperation(%q, %q) = %v, want %v", testcase.spec, testcase.operation, got, testcase.want)
		}
	}
}
