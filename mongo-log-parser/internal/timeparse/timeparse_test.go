package timeparse

import (
	"strings"
	"testing"
	"time"
)

func TestShortFormsTakeTheirDateFromTheBundle(t *testing.T) {
	day := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	got, err := Parse("10:00", day.Add(9*time.Hour), day.Add(11*time.Hour))
	if err != nil || !got.Equal(day.Add(10*time.Hour)) {
		t.Fatalf("10:00 → %v %v", got, err)
	}
	if _, err := Parse("10:00", day, day.Add(50*time.Hour)); err == nil || !strings.Contains(err.Error(), "say which day") {
		t.Fatalf("a clock in a three-day bundle must be refused, got %v", err)
	}
	got, err = Parse("01-16 01:00", day, day.Add(50*time.Hour))
	if err != nil || got.Day() != 16 || got.Year() != 2026 {
		t.Fatalf("01-16 01:00 → %v %v", got, err)
	}
	from, to, err := Window("", "", "2026-01-15 10:00", "20m", time.Time{}, time.Time{})
	if err != nil || to.Sub(from) != 20*time.Minute || from.Minute() != 50 {
		t.Fatalf("around: %v %v %v", from, to, err)
	}
}
