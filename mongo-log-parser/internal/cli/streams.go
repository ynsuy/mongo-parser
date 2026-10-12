package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"mongo-log-parser/internal/ftdc"
	"mongo-log-parser/internal/metrics"
	"mongo-log-parser/internal/render"
	"mongo-log-parser/internal/store"
)

// plural is a count and its noun, for a footer that reads as a sentence.

// streamSet is the FTDC streams a command was pointed at, opened once.
//
// Opening indexes chunk headers, so the commands that chart metrics share this rather than each
// walking the directories again for the span and then for the samples. Which directory belongs to
// which node was settled when the bundle was read, so a stream arrives here already named.
type streamSet struct {
	sources []string // what each stream is called in the output
	roles   []string // each stream's node role, from the store; empty when the bundle never said
	streams []*ftdc.Stream
	first   time.Time // earliest sample across the set
	last    time.Time // latest, which is what a relative window counts back from
	chunks  int

	// names is how each metric the bundle carries is shown, worked out once for the whole set: a
	// name has to be short enough to read and its own among the metrics on offer. Deciding it per
	// board would give one metric two names in two commands.
	names  map[string]string // metric path -> shown name
	byName map[string]string // shown name -> metric path
}

// openStreams opens the FTDC streams the caller asked for: the ones it named, or every one in the
// bundle when it named none or said "all".
func openStreams(st *store.Store, wanted []string) (*streamSet, error) {
	found, err := ftdcStreams(st, wanted)
	if err != nil {
		return nil, err
	}
	set := &streamSet{sources: make([]string, 0, len(found)), roles: make([]string, 0, len(found)), streams: make([]*ftdc.Stream, 0, len(found))}
	for _, f := range found {
		// Only the files ingest kept: the same directory can hold a copy it read once, or
		// whatever else sat beside the metrics files, and reading those would count samples twice.
		stream, err := ftdc.OpenFiles(f.dir, f.paths)
		if err != nil {
			// A stream the bundle holds and this build cannot read is a gap in the answer, not a
			// reason to refuse the question: coverage says what could not be read.
			continue
		}
		set.streams = append(set.streams, stream)
		set.sources = append(set.sources, f.label)
		set.roles = append(set.roles, f.role)
		set.chunks += stream.Chunks()
		first, last := stream.Span()
		if set.first.IsZero() || first.Before(set.first) {
			set.first = first
		}
		if last.After(set.last) {
			set.last = last
		}
	}
	return set, nil
}

// ftdcStream is one stream as the store settled it when it read the bundle.
type ftdcStream struct {
	dir   string
	label string   // a stream is named after the node it was written by
	role  string   // the node's process and cluster role, as far as the bundle said: "mongod (shard)"
	paths []string // the files ingest kept
}

// ftdcStreams is where each wanted stream's files are, and what to call it.
func ftdcStreams(st *store.Store, wanted []string) ([]ftdcStream, error) {
	rows, err := st.DB.Query(`
		SELECT ftdc.dir, coalesce(nullif(node.label, ''), nullif(node.name, ''), nullif(ftdc.host, ''), ftdc.origin),
		       coalesce(node.role, ''), ftdc.paths
		FROM ftdc LEFT JOIN node ON node.id = ftdc.node
		WHERE ftdc.chunks > 0 ORDER BY ftdc.first_ts, ftdc.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []ftdcStream
	for rows.Next() {
		var f ftdcStream
		var list string
		if err := rows.Scan(&f.dir, &f.label, &f.role, &list); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(list), &f.paths); err != nil {
			return nil, fmt.Errorf("FTDC stream %s: file list: %w", f.label, err)
		}
		all = append(all, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Naming nothing means the whole bundle. Refusing to read three nodes because the caller did
	// not say "all" made pointing at a path an error, when a path is the whole question.
	if len(wanted) == 0 || (len(wanted) == 1 && strings.EqualFold(wanted[0], "all")) {
		return all, nil
	}
	kept := make([]ftdcStream, 0, len(wanted))
	for _, want := range wanted {
		found := false
		for _, f := range all {
			if strings.EqualFold(f.label, want) || f.dir == want || strings.Contains(strings.ToLower(f.label), strings.ToLower(want)) {
				kept = append(kept, f)
				found = true
			}
		}
		if !found {
			names := make([]string, len(all))
			for i, f := range all {
				names[i] = f.label
			}
			return nil, fmt.Errorf("no FTDC stream here is called %s; this bundle has: %s", want, strings.Join(names, ", "))
		}
	}
	return kept, nil
}

// label is how one metric is shown, and metricPath turns a shown name back into the path it stands
// for, so a reader can type back what they were shown.
func (s *streamSet) label(path string) string {
	if name, ok := s.metricNames()[path]; ok {
		return name
	}
	return render.MetricName(path)
}

func (s *streamSet) metricPath(name string) string {
	if s.byName == nil {
		s.byName = map[string]string{}
		for path, shown := range s.metricNames() {
			s.byName[shown] = path
		}
	}
	if path, ok := s.byName[name]; ok {
		return path
	}
	return name
}

func (s *streamSet) metricNames() map[string]string {
	if s.names != nil {
		return s.names
	}
	// The curated metrics are named among themselves first, so their names do not change with how
	// many other paths a bundle happens to carry: a name a script or a reader learned stays the name.
	// Everything else is named after them, and one that would take a curated metric's name keeps
	// its full path instead.
	curated, rest := make([]string, 0, 512), make([]string, 0, 2048)
	for _, path := range s.paths(time.Time{}, time.Time{}) {
		if metrics.Curated(path) {
			curated = append(curated, path)
		} else {
			rest = append(rest, path)
		}
	}
	s.names = render.MetricNames(curated)
	taken := make(map[string]bool, len(s.names))
	for _, name := range s.names {
		taken[name] = true
	}
	for path, name := range render.MetricNames(rest) {
		if taken[name] {
			name = path
		}
		s.names[path] = name
	}
	return s.names
}

// spans is each stream's coverage, for a footer that says what was actually read.
func (s *streamSet) spans() []string {
	out := make([]string, len(s.streams))
	for i, stream := range s.streams {
		first, last := stream.Span()
		out[i] = first.Format(time.RFC3339) + " .. " + last.Format(time.RFC3339)
	}
	return out
}

// read decodes one window from every stream and settles which metrics will be charted: the ones
// named, or the standard overview when nothing was. The window is the same for all of them, so two
// nodes' charts describe one period.
//
// also is decoded beside whatever the request would have decoded: the metrics the words after the
// path picked, which may be any of the thousands a stream carries.
func (s *streamSet) read(from, to time.Time, named, also []string, done func(chunks, total int)) ([]*ftdc.Samples, []string, error) {
	named, err := s.expandMetrics(s.resolveMetrics(named), from, to)
	if err != nil {
		return nil, nil, err
	}
	want := selector(named, also)
	samples := make([]*ftdc.Samples, len(s.streams))
	for i, stream := range s.streams {
		read, err := stream.Read(from, to, want, done)
		if err != nil {
			return nil, nil, err
		}
		samples[i] = read
	}
	if len(named) > 0 {
		return samples, named, nil
	}
	return samples, metrics.Overview(carried(samples)), nil
}

// damageNote says what a read could not decode, naming the node it was lost from. A chunk that
// did not survive the collection is a stretch of one node's charts that is missing rather than
// flat, and the difference matters to anyone reading a dip in them.
func damageNote(samples []*ftdc.Samples, sources []string) string {
	lines := make([]string, 0, len(samples))
	for i, read := range samples {
		source := "this stream"
		if i < len(sources) {
			source = sources[i]
		}
		if line := damageLine(read, source); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "damaged diagnostic data\n  " + strings.Join(lines, "\n  ")
}

// damageLine is one stream's share of that, empty when the stream decoded whole.
func damageLine(read *ftdc.Samples, source string) string {
	if read == nil || read.Skipped == 0 {
		return ""
	}
	return fmt.Sprintf("%s: %s could not be decoded and %s missing from its charts (%s)",
		source, plural(read.Skipped, "FTDC chunk"), isAre(read.Skipped), strings.Join(read.Damaged, "; "))
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// carried is every metric path these samples hold, in a stable order.
func carried(samples []*ftdc.Samples) []string {
	seen := map[string]bool{}
	paths := make([]string, 0, 256)
	for _, read := range samples {
		for path := range read.Values {
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

// resolveMetrics turns what the caller typed into the paths the streams carry.
//
// A metric is shown as opcounters.query, so that is what a reader types back. Those names are made
// not to collide — one that would share a tail with another grows until it does not — so this is a
// lookup and not a guess: nothing here has to report an ambiguity, because none is ever created.
// A full path still works, which is what an older script already has.
func (s *streamSet) resolveMetrics(named []string) []string {
	if len(named) == 0 {
		return nil
	}
	resolved := make([]string, len(named))
	for i, want := range named {
		resolved[i] = s.metricPath(want)
	}
	return resolved
}

// globLimit caps how many metrics one pattern may stand for. A subtree is a fair thing to ask for
// whole; `*` on its own is a thousand charts nobody can read, and saying so with the count is more
// use than either drawing them or quietly keeping the first few.
const globLimit = 100

// hasGlob reports whether a name is a pattern to match rather than a metric to look up.
func hasGlob(name string) bool { return strings.ContainsAny(name, "*?") }

// globMatch reports whether a metric answers to a pattern like serverStatus/metrics/document/*.
//
// * stands for any run of characters, / included, and ? for exactly one. Letting * cross a segment
// boundary is the reading a person means: globbing a subtree asks for everything under it, not only
// the next level down. Matching is case-insensitive, like every other name this tool takes back.
func globMatch(pattern, name string) bool {
	p, n := strings.ToLower(pattern), strings.ToLower(name)
	// Two pointers with one backtrack point: at a *, remember where it stood and how far the name
	// had come, and on a later mismatch let it swallow one more character and try again.
	pi, ni, star, swallowed := 0, 0, -1, 0
	for ni < len(n) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == n[ni]):
			pi++
			ni++
		case pi < len(p) && p[pi] == '*':
			star, swallowed = pi, ni
			pi++
		case star >= 0:
			swallowed++
			pi, ni = star+1, swallowed
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// expandMetrics turns the patterns among the named metrics into the paths the streams carry.
//
// --metric serverStatus/metrics/document/* is a reader asking for a subtree they were just shown,
// and answering it with "no metric named that" makes them type out four paths the tool already
// knows. A pattern is matched against the full path and against the shorter name the metric is
// shown under, so either of the two things a reader has in front of them works. Names with no
// pattern in them pass straight through, so a lookup stays a lookup.
func (s *streamSet) expandMetrics(named []string, from, to time.Time) ([]string, error) {
	patterned := false
	for _, want := range named {
		patterned = patterned || hasGlob(want)
	}
	if !patterned {
		return named, nil
	}
	available := s.paths(from, to)

	expanded := make([]string, 0, len(named))
	seen := map[string]bool{}
	add := func(path string) {
		if !seen[path] {
			seen[path] = true
			expanded = append(expanded, path)
		}
	}
	for _, want := range named {
		if !hasGlob(want) {
			add(want)
			continue
		}
		matched, err := globPaths(want, available, s.label)
		if err != nil {
			return nil, err
		}
		for _, path := range matched {
			add(path)
		}
	}
	return expanded, nil
}

// globPaths is every metric a pattern stands for, in the order the names came in.
//
// Matching a pattern is only half of answering one: nothing matched and too much matched are both
// answers a reader can act on, and both are wrong to report as a chart. label is how each path is
// shown, so a pattern written against the short names finds them too.
func globPaths(pattern string, available []string, label func(string) string) ([]string, error) {
	// The pattern is matched against any part of a name, the way a plain word after the path is.
	// Nearly every path opens with serverStatus/ and nobody types it, so anchoring a pattern to the
	// whole path would answer document/* with nothing and metrics/document/* with everything.
	loose := pattern
	if !strings.HasPrefix(loose, "*") {
		loose = "*" + loose
	}
	if !strings.HasSuffix(loose, "*") {
		loose += "*"
	}
	matched := make([]string, 0, 16)
	for _, path := range available {
		if globMatch(loose, path) || globMatch(loose, label(path)) {
			matched = append(matched, path)
		}
	}
	if len(matched) == 0 {
		message := fmt.Sprintf("nothing matched %s in this bundle", pattern)
		if nearest := suggest(available, literalOf(pattern), 6); len(nearest) > 0 {
			message += "; did you mean:\n  " + strings.Join(nearest, "\n  ")
		} else {
			message += fmt.Sprintf("; the streams carry %d metrics, list them with --list", len(available))
		}
		return nil, fmt.Errorf("%s", message)
	}
	if len(matched) > globLimit {
		return nil, fmt.Errorf("%s matches %d metrics, more than the %d one set of charts can show; narrow the pattern, or see them all with --list",
			pattern, len(matched), globLimit)
	}
	return matched, nil
}

// literalOf is a pattern with its wildcard segments dropped, which is what the nearest names to a
// miss are ranked against: scoring a segment of "*" makes every metric in the bundle a match.
func literalOf(pattern string) string {
	segments := strings.Split(pattern, "/")
	kept := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment != "" && !hasGlob(segment) {
			kept = append(kept, segment)
		}
	}
	return strings.Join(kept, "/")
}

// paths is every metric name the streams carry in a window, across the set and in a stable order.
//
// Read on a filtered request only materialises what was asked for, so this is what answers "what
// else is there" — for a pattern to expand against, and for the nearest names to a miss.
func (s *streamSet) paths(from, to time.Time) []string {
	seen := map[string]bool{}
	all := make([]string, 0, 512)
	for _, stream := range s.streams {
		paths, err := stream.Paths(from, to)
		if err != nil {
			continue
		}
		for _, path := range paths {
			if !seen[path] {
				seen[path] = true
				all = append(all, path)
			}
		}
	}
	sort.Strings(all)
	return all
}

// selector is the decode filter for a request: the metrics that were named, or the curated set
// ranking chooses from, and whatever else was asked for alongside. Everything else is skipped
// before it is materialised — not because it is not wanted, but because a week of every column a
// stream carries is gigabytes, and any one of them is decoded the moment someone names it.
func selector(named, also []string) ftdc.Selector {
	wanted := make(map[string]bool, len(named)+len(also))
	for _, path := range also {
		wanted[path] = true
	}
	if len(named) == 0 {
		return func(path string) bool { return wanted[path] || metrics.Charted(path) }
	}
	for _, path := range named {
		wanted[path] = true
	}
	return func(path string) bool { return wanted[path] }
}

// available is every metric the streams carry in a window, the charted ones first. A word that
// matches by substring is capped, and the capped list should be the metrics someone usually means
// before the hundreds of counters nobody looks at.
func (s *streamSet) available(from, to time.Time) []string {
	all := s.paths(from, to)
	ordered := make([]string, 0, len(all))
	for _, path := range all {
		if metrics.Charted(path) {
			ordered = append(ordered, path)
		}
	}
	for _, path := range all {
		if !metrics.Charted(path) {
			ordered = append(ordered, path)
		}
	}
	return ordered
}

// reportUnknownMetrics turns a name that matched nothing into an answer.
//
// A metric absent from every stream is almost always a name that was typed or guessed wrong, and
// charting it as "no samples in this window" tells the reader the server was idle when really the
// question never landed. Saying so, with the nearest names the bundle does carry, is the difference
// between a wrong reading and a corrected one.
func (s *streamSet) reportUnknownMetrics(named []string, samples []*ftdc.Samples, from, to time.Time) error {
	missing := make([]string, 0, len(named))
	for _, path := range named {
		found := false
		for _, read := range samples {
			if _, ok := read.Values[path]; ok {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, path)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	names := s.paths(from, to)
	available := make(map[string]bool, len(names))
	for _, path := range names {
		available[path] = true
	}
	unknown := make([]string, 0, len(missing))
	for _, path := range missing {
		if !available[path] {
			unknown = append(unknown, path)
		}
	}
	if len(unknown) == 0 {
		// The names exist; they simply have no samples here, which the chart says for itself.
		return nil
	}

	message := fmt.Sprintf("no metric named %s in this bundle", strings.Join(unknown, ", "))
	if nearest := suggest(names, unknown[0], 6); len(nearest) > 0 {
		message += "; did you mean:\n  " + strings.Join(nearest, "\n  ")
	} else {
		message += fmt.Sprintf("; the streams carry %d metrics, run without --metric to see the standard board", len(names))
	}
	return fmt.Errorf("%s", message)
}

// suggest ranks the available names by how many parts of the requested path they contain, so a near
// miss on one segment still finds its neighbours.
//
// The first segment is dropped before scoring. Nearly every path begins with serverStatus, so
// counting it makes every metric in the bundle a match and turns the suggestions into six arbitrary
// names — worse than admitting there is nothing close.
func suggest(available []string, want string, limit int) []string {
	parts := strings.FieldsFunc(strings.ToLower(want), func(r rune) bool { return r == '/' })
	if len(parts) > 1 {
		parts = parts[1:]
	}
	type scored struct {
		path  string
		score int
	}
	ranked := make([]scored, 0, len(available))
	for _, path := range available {
		lower := strings.ToLower(path)
		score := 0
		for _, part := range parts {
			if len(part) > 2 && strings.Contains(lower, part) {
				score++
			}
		}
		if score > 0 {
			ranked = append(ranked, scored{path, score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	names := make([]string, 0, limit)
	for _, item := range ranked {
		if len(names) == limit {
			break
		}
		names = append(names, item.path)
	}
	return names
}

// resolvedNames is the paths a request settled on, or nothing when the request named no metrics and
// the order came from ranking instead.
func resolvedNames(named, order []string) []string {
	if len(named) == 0 {
		return nil
	}
	return order
}
