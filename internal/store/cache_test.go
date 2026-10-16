package store

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestExpired(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	day := 24 * time.Hour
	const gib = int64(1) << 30
	item := func(dir string, gb int64, idle time.Duration) Cached {
		return Cached{Dir: dir, Bundle: "/cases/" + dir, Bytes: gb * gib, LastUsed: ago(idle)}
	}
	exists := func(string) bool { return true }
	dirs := func(items []Cached) []string {
		out := []string{}
		for _, item := range items {
			out = append(out, item.Dir)
		}
		return out
	}

	cases := []struct {
		name     string
		items    []Cached
		maxAge   time.Duration
		maxBytes int64
		exists   func(string) bool
		want     []string
	}{
		{"old stores go, recent stay", []Cached{item("old", 1, 15*day), item("week", 1, 7*day)},
			14 * day, 20 * gib, exists, []string{"old"}},
		{"the new store and one in use stay however old", []Cached{item("new", 1, 30*day), item("open", 1, time.Minute)},
			time.Minute, 20 * gib, exists, []string{}},
		{"a gone bundle's store goes", []Cached{item("moved", 1, 2*time.Hour), item("here", 1, 2*time.Hour)},
			14 * day, 20 * gib, func(path string) bool { return path != "/cases/moved" }, []string{"moved"}},
		{"over the cap, least recently used first", []Cached{item("big", 8, 2*day), item("a", 8, 5*day), item("b", 8, 3*day)},
			14 * day, 20 * gib, exists, []string{"a"}},
		{"the cap never takes the new store or one in use", []Cached{item("new", 30, 3*day), item("open", 5, time.Minute), item("c", 1, day)},
			14 * day, 20 * gib, exists, []string{"c"}},
		{"an interrupted read goes only by age", []Cached{{Dir: "partial", Bundle: interrupted, Bytes: 30 * gib, LastUsed: ago(2 * day)}},
			14 * day, 20 * gib, exists, []string{}},
		{"zero turns both off", []Cached{item("old", 30, 90*day)},
			0, 0, func(string) bool { return false }, []string{}},
	}
	for _, c := range cases {
		got := dirs(expired(c.items, "new", now, c.maxAge, c.maxBytes, c.exists))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: removed %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTidyRemovesFromTheCache(t *testing.T) {
	t.Setenv("MONGO_PARSER_CACHE", t.TempDir())
	t.Setenv("MONGO_PARSER_KEEP_DAYS", "14")
	old, fresh := Dir("/cases/old"), Dir("/cases/fresh")
	for _, dir := range []string{old, fresh} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	month := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, month, month); err != nil {
		t.Fatal(err)
	}
	removed, err := Tidy(fresh)
	if err != nil || len(removed) != 1 || removed[0].Dir != old {
		t.Fatalf("removed %v, err %v", removed, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("%s still there", filepath.Base(old))
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal(err)
	}
}
