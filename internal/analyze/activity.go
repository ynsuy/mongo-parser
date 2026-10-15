package analyze

import (
	"sort"
	"time"
)

// A table of counts cannot say when. Ten thousand errors spread evenly over two days is a
// deployment logging something harmless; the same ten thousand inside four minutes is the incident,
// and the two print identically in every other section of a report. Activity is that difference,
// and it is what lets the rest of the report be read against a moment rather than a window.

// Activity is how much was logged over time, split by how bad it was. Each series is a sparse
// per-minute history, so a quiet week costs nothing to keep.
type Activity struct {
	All      Timeline `json:"-"`
	Errors   Timeline `json:"-"`
	Warnings Timeline `json:"-"`

	// Severities is the whole window's count per severity letter, which is the one-line version.
	Severities map[string]int `json:"severities"`
	// Categories is the same for the coarse buckets, so "what kind of thing was this log full of"
	// has an answer without ranking every group.
	Categories map[Category]int `json:"categories"`

	// Records, FirstAt and LastAt are what this pass actually read. They are not the bundle's own
	// span: under a window they are the window's, and a report that prints the bundle's span over
	// counts taken from a slice of it invites every figure below to be read against the whole.
	Records int       `json:"records"`
	FirstAt time.Time `json:"first_at,omitzero"`
	LastAt  time.Time `json:"last_at,omitzero"`

	byCategory map[Category]Timeline
}

func NewActivity() *Activity {
	return &Activity{
		All: Timeline{}, Errors: Timeline{}, Warnings: Timeline{},
		Severities: map[string]int{}, Categories: map[Category]int{},
		byCategory: map[Category]Timeline{},
	}
}

// Add counts one record.
func (a *Activity) Add(record Record) {
	a.Records++
	if a.FirstAt.IsZero() || record.At.Before(a.FirstAt) {
		a.FirstAt = record.At
	}
	if record.At.After(a.LastAt) {
		a.LastAt = record.At
	}
	a.All.Add(record.At, 0, 0)
	a.Severities[record.Sev]++
	switch record.Sev {
	case "F", "E":
		a.Errors.Add(record.At, 0, 0)
	case "W":
		a.Warnings.Add(record.At, 0, 0)
	}
	category := Classify(record)
	a.Categories[category]++
	timeline := a.byCategory[category]
	if timeline == nil {
		timeline = Timeline{}
		a.byCategory[category] = timeline
	}
	timeline.Add(record.At, 0, 0)
}

// Category returns the history of one bucket, for a chart that splits the lines by what they were
// about rather than by how bad they were.
func (a *Activity) Category(category Category) Timeline { return a.byCategory[category] }

// Busiest names the step in which the most lines of the given history were written, which is the
// moment worth looking at first. The step is the start of a TimelineStep bin.
func Busiest(timeline Timeline) (at time.Time, count int) {
	step := int64(0)
	for key, bin := range timeline {
		if bin.Count > count || (bin.Count == count && key < step) {
			step, count = key, bin.Count
		}
	}
	if count == 0 {
		return time.Time{}, 0
	}
	return time.Unix(step, 0).UTC(), count
}

// TopCategories ranks the buckets by how much was logged in them.
func (a *Activity) TopCategories(limit int) []Category {
	out := make([]Category, 0, len(a.Categories))
	for category := range a.Categories {
		out = append(out, category)
	}
	sort.Slice(out, func(i, j int) bool {
		if a.Categories[out[i]] != a.Categories[out[j]] {
			return a.Categories[out[i]] > a.Categories[out[j]]
		}
		return out[i] < out[j]
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Burst is one category whose busiest minute stood far above its own typical minute.
//
// It is a statement about shape, not about volume. A server that writes sixty thousand connection
// lines a day is busy; one that writes thirty times its usual minute inside one minute had
// something happen to it, and those two print identically in a table of totals. Nothing here says
// what happened — only that the log stopped being steady, and when.
type Burst struct {
	Category Category  `json:"category"`
	At       time.Time `json:"at,omitzero"`
	Peak     int       `json:"peak"`
	Baseline float64   `json:"baseline"`
	Ratio    float64   `json:"ratio"`
}

// burstNeighbourhood is how many bins either side of the peak make up the baseline it is read
// against, in TimelineStep units — an hour centred on the peak.
//
// The baseline is local on purpose. A bundle can be several exports stitched together: one
// covering an incident minute by minute, another covering a quiet stretch some time later, and
// nothing in between. A median taken over the whole span is then a median of that gap, and every busy minute
// in every export reads as a burst against it. Measured against the minutes either side of it, a
// peak is a burst when it broke the shape of its own hour, which is what the word means.
const burstNeighbourhood = 30

// BurstRule is what counts as a burst. The thresholds are there to keep the rule quiet rather than
// to catch everything, and each one answers a way the arithmetic goes wrong:
//
//   - MinBins: a neighbourhood of three minutes has no median worth the name.
//   - MinPeak: a peak of four lines is not a moment anybody needs pointing at.
//   - MinBaseline: a category that writes one line a minute, sometimes, has no usual minute to
//     break. Dividing into it turns an ordinary batch of messages into thousands of times the usual
//     minute, which is not a finding about the server but about the median being 1.
//   - MinRatio: below it, the shape is ordinary variation in a busy log.
type BurstRule struct {
	MinBins     int
	MinPeak     int
	MinBaseline int
	MinRatio    float64
}

// Bursts names the categories whose peak minute stood at least MinRatio times the minutes around
// it. A quiet bundle reports nothing, and a busy one is not punished for being busy.
//
// A bundle holding only the incident has no quiet minutes to compare against and so reports nothing
// — which is correct, and is what coverage is for.
func (a *Activity) Bursts(rule BurstRule) []Burst {
	out := make([]Burst, 0, len(a.byCategory))
	for category, timeline := range a.byCategory {
		moment, peak := Busiest(timeline)
		if peak < rule.MinPeak {
			continue
		}
		step := moment.Unix()
		counts := make([]int, 0, 2*burstNeighbourhood+1)
		for key, bin := range timeline {
			if bin.Count == 0 {
				continue
			}
			if away := (key - step) / TimelineStep; away >= -burstNeighbourhood && away <= burstNeighbourhood {
				counts = append(counts, bin.Count)
			}
		}
		if len(counts) < rule.MinBins {
			continue
		}
		sort.Ints(counts)
		median := counts[len(counts)/2]
		if median < rule.MinBaseline {
			continue
		}
		baseline := float64(median)
		if ratio := float64(peak) / baseline; ratio >= rule.MinRatio {
			out = append(out, Burst{category, moment, peak, baseline, ratio})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ratio != out[j].Ratio {
			return out[i].Ratio > out[j].Ratio
		}
		return out[i].Category < out[j].Category
	})
	return out
}

// Problems is how many lines the server itself called a warning or worse.
func (a *Activity) Problems() (errors, warnings int) {
	return a.Severities["F"] + a.Severities["E"], a.Severities["W"]
}
