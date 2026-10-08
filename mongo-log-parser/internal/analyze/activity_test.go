package analyze

import "testing"

// A total over a window cannot tell a steady background from an incident. These two stretches hold
// the same number of error lines: one drips them over an hour, the other writes them all in a
// minute.
func TestActivityFindsTheMinuteTheErrorsHappenedIn(t *testing.T) {
	steady, burst := "", ""
	for i := range 30 {
		steady += `{"t":{"$date":"2026-09-03T10:` + twoDigits(i) + `:00Z"},"s":"E","c":"NETWORK","id":1,"ctx":"c","msg":"steady"}` + "\n"
		burst += `{"t":{"$date":"2026-09-03T11:05:` + twoDigits(i) + `Z"},"s":"E","c":"NETWORK","id":2,"ctx":"c","msg":"burst"}` + "\n"
	}

	activity := NewActivity()
	feed(t, "node-a", steady+burst, activity)

	if errors, warnings := activity.Problems(); errors != 60 || warnings != 0 {
		t.Errorf("errors %d, warnings %d; want 60 and 0", errors, warnings)
	}
	busiest, count := Busiest(activity.Errors)
	if count != 30 {
		t.Errorf("busiest minute held %d errors, want the 30 of the burst", count)
	}
	if busiest.Format("15:04") != "11:05" {
		t.Errorf("busiest minute = %s, want 11:05", busiest.Format("15:04"))
	}
}

func twoDigits(value int) string {
	if value < 10 {
		return "0" + string(rune('0'+value))
	}
	return string(rune('0'+value/10)) + string(rune('0'+value%10))
}

func TestActivityCountsSeveritiesAndCategories(t *testing.T) {
	activity := NewActivity()
	feed(t, "node-a", lines, activity)

	if activity.Severities["I"] != 3 || activity.Severities["W"] != 1 {
		t.Errorf("severities = %v, want 3 I and 1 W", activity.Severities)
	}
	if activity.Categories[Network] != 2 || activity.Categories[SlowQuery] != 1 {
		t.Errorf("categories = %v, want 2 network and 1 slow_query", activity.Categories)
	}
	if top := activity.TopCategories(1); len(top) != 1 || top[0] != Network {
		t.Errorf("top category = %v, want network", top)
	}
}

// networkLines writes count NETWORK lines inside one minute, at severity I. Connection traffic is
// the case this rule exists for and the server never calls it a problem: every line is an I.
func networkLines(day, hour, minute, count int) string {
	out := ""
	for i := range count {
		out += `{"t":{"$date":"2026-09-` + twoDigits(day) + `T` + twoDigits(hour) + `:` + twoDigits(minute) +
			`:` + twoDigits(i%60) + `Z"},"s":"I","c":"NETWORK","id":22943,"ctx":"c","msg":"client metadata"}` + "\n"
	}
	return out
}

// The case the rule exists for: a steady log that stops being steady, with nothing above severity I
// anywhere in it. A rule that watches severity says nothing here, and a table of totals says the
// server wrote a lot of connection lines, which it does every day.
func TestABurstIsAMinuteThatBrokeTheShapeOfItsHour(t *testing.T) {
	text := ""
	for minute := range 40 {
		text += networkLines(3, 10, minute, 20)
	}
	text += networkLines(3, 10, 20, 180) // on top of that minute's own 20

	activity := NewActivity()
	feed(t, "node-a", text, activity)

	bursts := activity.Bursts(BurstRule{MinBins: 10, MinPeak: 100, MinBaseline: 5, MinRatio: 5})
	if len(bursts) != 1 {
		t.Fatalf("got %d bursts, want the one at 10:20: %+v", len(bursts), bursts)
	}
	if got := bursts[0].Category; got != Network {
		t.Errorf("category %q, want network", got)
	}
	if got := bursts[0].At.Format("15:04"); got != "10:20" {
		t.Errorf("burst at %s, want 10:20", got)
	}
	if bursts[0].Peak != 200 || bursts[0].Baseline != 20 {
		t.Errorf("peak %d against baseline %v; want 200 against 20", bursts[0].Peak, bursts[0].Baseline)
	}
}

// A steady log is not a burst however much of it there is.
func TestASteadyLogReportsNoBurst(t *testing.T) {
	text := ""
	for minute := range 40 {
		text += networkLines(3, 10, minute, 200)
	}
	activity := NewActivity()
	feed(t, "node-a", text, activity)

	if bursts := activity.Bursts(BurstRule{MinBins: 10, MinPeak: 100, MinBaseline: 5, MinRatio: 5}); len(bursts) != 0 {
		t.Errorf("a flat 200 lines a minute reported %d bursts: %+v", len(bursts), bursts)
	}
}

// A bundle can be several exports stitched together — one covering an incident, another covering
// a quiet stretch some time later, and nothing in between. Measured against a median of the whole
// span, every minute of the busier export reads as a burst, and the finding points at an artefact of
// how the bundle was collected.
//
// Here the second export is simply busier, minute after minute, and nothing in it broke its shape.
func TestABusierSecondExportIsNotABurst(t *testing.T) {
	text := ""
	for minute := range 45 {
		text += networkLines(3, 10, minute, 20) // the first export: quiet
	}
	for minute := range 15 {
		text += networkLines(4, 11, minute, 200) // a day later, ten times busier and perfectly flat
	}
	activity := NewActivity()
	feed(t, "node-a", text, activity)

	if bursts := activity.Bursts(BurstRule{MinBins: 10, MinPeak: 100, MinBaseline: 5, MinRatio: 5}); len(bursts) != 0 {
		t.Errorf("a second, busier export reported %d bursts: %+v", len(bursts), bursts)
	}
}

// A bundle holding only the incident has no quiet minutes to be read against. Saying nothing is
// correct: the alternative is a ratio computed from the incident's own minutes.
func TestAnIncidentOnlyBundleReportsNoBurst(t *testing.T) {
	text := ""
	for minute := range 6 {
		text += networkLines(3, 10, minute, 500)
	}
	activity := NewActivity()
	feed(t, "node-a", text, activity)

	if bursts := activity.Bursts(BurstRule{MinBins: 10, MinPeak: 100, MinBaseline: 5, MinRatio: 5}); len(bursts) != 0 {
		t.Errorf("six minutes of incident reported %d bursts: %+v", len(bursts), bursts)
	}
}

// A category that writes a line now and then, and once wrote a batch, has a median minute of one
// line. A "burst" against it is a fact about the median, not about the server.
func TestASporadicCategoryHasNoUsualMinuteToBreak(t *testing.T) {
	text := ""
	for minute := range 40 {
		text += networkLines(3, 10, minute, 1) // a line a minute, which is not a steady level
	}
	text += networkLines(3, 10, 20, 500)

	activity := NewActivity()
	feed(t, "node-a", text, activity)

	rule := BurstRule{MinBins: 10, MinPeak: 100, MinBaseline: 5, MinRatio: 5}
	if bursts := activity.Bursts(rule); len(bursts) != 0 {
		t.Errorf("a median minute of 1 reported %d bursts: %+v", len(bursts), bursts)
	}
	// Without the floor the same history divides into a ratio of 501.
	loose := rule
	loose.MinBaseline = 0
	if bursts := activity.Bursts(loose); len(bursts) != 1 {
		t.Fatalf("without the floor this should still fire; got %d", len(bursts))
	}
}
