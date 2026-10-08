package store

import (
	"path/filepath"
	"testing"
)

func TestBlocksRoundTripAndMergeStreamsByTime(t *testing.T) {
	st, err := Create(filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(id, stream int64, times ...int64) {
		var records []Record
		for i, ts := range times {
			r := Record{TS: ts, Sev: "I", Msg: "m", Raw: `{"t":1}`, Line: i + 1}
			if stream == 2 {
				r.SetAttr(`{"x":1}`)
			}
			records = append(records, r)
		}
		if _, err := st.DB.Exec(`INSERT INTO block(id, stream, first_ts, last_ts, lines, sev_mask, ids, data) VALUES (?,?,?,?,?,?,?,?)`,
			id, stream, times[0], times[len(times)-1], len(times), SevBit("I"), ",0,", EncodeBlock(records)); err != nil {
			t.Fatal(err)
		}
	}
	put(1, 1, 10, 30, 50)
	put(2, 2, 20, 40, 60)
	put(3, 1, 70, 90)
	var got []int64
	if err := st.Scan(Query{}, func(r *Record) bool { got = append(got, r.TS); return true }); err != nil {
		t.Fatal(err)
	}
	want := []int64{10, 20, 30, 40, 50, 60, 70, 90}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	got = nil
	_ = st.Scan(Query{Newest: true, From: 25, To: 65}, func(r *Record) bool { got = append(got, r.TS); return true })
	if len(got) != 4 || got[0] != 60 || got[3] != 30 {
		t.Fatalf("newest first in 25..65: %v", got)
	}
	line, err := st.Line(Ref{Block: 2, Slot: 1})
	if err != nil || line.TS != 40 || string(line.Attr()) != `{"x":1}` {
		t.Fatalf("line %+v err %v", line, err)
	}
}
