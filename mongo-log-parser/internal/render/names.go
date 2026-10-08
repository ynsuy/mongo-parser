package render

import (
	"strings"

	"mongo-log-parser/internal/series"
)

// MetricName is how a metric path is shown: the two segments that identify it, joined by a dot.
//
//	serverStatus/opcounters/query                            -> opcounters.query
//	serverStatus/wiredTiger/concurrentTransactions/write/out -> write.out
//
// Nearly every path begins serverStatus/, and the middle of a path says which subsystem MongoDB
// files a number under rather than what the number is. The last two segments are what tells one
// metric from its neighbours.
//
// Two segments are not always enough to tell one metric from another, so this is the name for a
// path considered alone. MetricNames is the one to use when the whole set is known: it lengthens
// the names that would collide until every one of them is its own.
func MetricName(path string) string { return suffixName(path, 2) }

// MetricNames is the name for every path in a set: the shortest form that no other path in the set
// shares, never fewer than two segments.
//
// Naming metrics by their last two segments reads far better than printing the whole path, but
// queryExecutor/collectionScans/total and queryExecutor/profiler/collectionScans/total both end
// collectionScans/total. Reporting that as a clash pushed the problem onto the reader; growing the
// name until it is its own answers it instead, and leaves every name safe to type back.
//
// Growing only the tail is not always the short way. Per-collection stats differ in their first
// segment — config.image_collection.stats against config.transactions.stats — and everything after
// it can be identical, so a tail long enough to separate them carries the whole path. For those the
// head is what identifies the metric, and it is put back in front of the tail instead.
func MetricNames(paths []string) map[string]string {
	names := make(map[string]string, len(paths))
	remaining := append([]string(nil), paths...)
	for _, form := range nameForms {
		if len(remaining) == 0 {
			break
		}
		byName := make(map[string][]string, len(remaining))
		for _, path := range remaining {
			byName[form(path)] = append(byName[form(path)], path)
		}
		next := remaining[:0:0]
		for name, sharing := range byName {
			if len(sharing) == 1 {
				names[sharing[0]] = name
				continue
			}
			next = append(next, sharing...)
		}
		remaining = next
	}
	// Whole paths cannot collide, so anything still unnamed takes its own.
	for _, path := range remaining {
		names[path] = strings.ReplaceAll(path, "/", ".")
	}
	return names
}

// nameForms are the ways to name a path, shortest and plainest first. The first one that nothing
// else in the set answers to is the name.
var nameForms = []func(string) string{
	func(p string) string { return suffixName(p, 2) },
	func(p string) string { return suffixName(p, 3) },
	func(p string) string { return suffixName(p, 4) },
	func(p string) string { return headAndSuffix(p, 2) },
	func(p string) string { return headAndSuffix(p, 3) },
	func(p string) string { return headAndSuffix(p, 4) },
}

// suffixName is the last depth segments of a path, joined by dots.
func suffixName(path string, depth int) string {
	segments := strings.Split(path, "/")
	if len(segments) <= depth {
		return strings.Join(segments, ".")
	}
	return strings.Join(segments[len(segments)-depth:], ".")
}

// headAndSuffix is the first segment and the last depth of them, with the skipped middle marked.
func headAndSuffix(path string, depth int) string {
	segments := strings.Split(path, "/")
	if len(segments) <= depth+1 {
		return strings.Join(segments, ".")
	}
	return segments[0] + "…" + strings.Join(segments[len(segments)-depth:], ".")
}

// Sources is the line naming every stream on a board and the colour it was drawn in.
//
// It is the same line above a report and a metrics table: whatever else those
// three show, the reader has to be able to tell which node a line belongs to, and they should not
// have to learn a different key for each command.
func Sources(rows []series.Series, color bool) string {
	paint := painter(color)
	names := sourcesOf(rows)
	if len(names) == 0 {
		return ""
	}
	colors, pens := sourceColors(rows), sourceStrokes(rows)
	parts := make([]string, len(names))
	for i, source := range names {
		// The key carries the glyph as well as the colour: on a chart where several streams are
		// drawn over one another, the shape of a line is what tells them apart without colour.
		rule := strings.Repeat(string(pens[source].flat), 2)
		parts[i] = paint(rule+" "+source, colors[source])
	}
	return strings.Join(parts, "   ")
}

// nameOf is what to print for a series: the label it was given, or a name worked out from the path
// alone when nothing gave it one.
func nameOf(row series.Series) string {
	if row.Label != "" {
		return row.Label
	}
	return MetricName(row.Metric)
}
