package label

import (
	"bytes"
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The rule files are YAML because people write them: a rule needs a comment beside a threshold
// saying where the number came from, and JSON has nowhere to put one.
//
//go:embed rules/*.yaml
var ruleFiles embed.FS

// Pack is one rule file: the rules for one subject.
type Pack struct {
	Pack        string `yaml:"pack"`
	Description string `yaml:"description"`
	Rules       []Rule `yaml:"rules"`
}

// Rule is one label: what it is computed from, which summary of each minute it is read on, and the
// bands it is read against.
type Rule struct {
	// Pack is the rule file's subject — disk, replication, storage-engine — set when it is loaded.
	Pack  string `json:"pack" yaml:"-"`
	Name  string `json:"name" yaml:"name"`
	Title string `json:"title" yaml:"title"`
	// Doc says what the number means and how to read its levels. It is what a model reads.
	Doc    string `json:"doc" yaml:"doc"`
	Signal Signal `json:"signal" yaml:"signal"`
	// View is the summary of each minute the bands and the shape are read on: mean, max, min,
	// time_above or time_below. Empty is mean.
	View string `json:"view" yaml:"view"`
	// Threshold is the signal value a time view counts seconds past, in the signal's unit.
	Threshold *float64 `json:"threshold,omitempty" yaml:"threshold"`
	// Levels are the fuzzy bands, lowest first. A rule with none is read for its shape only:
	// operations a second or IOPS have no level that is high for every server, but a step or an
	// hourly burst in them is still the story.
	Levels []Level `json:"levels,omitempty" yaml:"levels"`
	// Direction "down" looks for stretches below the usual level rather than above it: a flow
	// control limit that drops, a secondary whose apply rate falls away.
	Direction string `json:"direction,omitempty" yaml:"direction"`
	// Elevated is the view value at or past which a minute counts towards the duty cycle: how much
	// of the window the label spent somewhere it should not stay.
	Elevated *float64 `json:"elevated,omitempty" yaml:"elevated"`
	// MinChange is the smallest move, in the view's unit, that counts as a change at all. It keeps a
	// disk that went from 1% to 3% busy from being reported as tripling.
	MinChange float64 `json:"min_change" yaml:"min_change"`
	// PairsWith names the labels to read beside this one when it is opened up: the pool size beside
	// how scarce the pool was, the wait time beside the queue.
	PairsWith []string `json:"pairs_with,omitempty" yaml:"pairs_with"`
	// OpenAt is the band from which a peak is worth opening the label up for. Empty is the top two
	// bands of four or more, or the top band of fewer. A pool with no ticket left for a single second
	// is worth opening; a CPU pinned for a single second is not, and this is where that is said.
	OpenAt string `json:"open_at,omitempty" yaml:"open_at"`
	// OpenWith names labels that must agree before this one is worth opening: at least one of them,
	// on the same node and the same instance, has to have left its lowest band. A device busy every
	// second of the day that still answers in a tenth of a millisecond is not a device anything waits
	// on, and disk.busy says so only beside disk.await.
	OpenWith []string `json:"open_with,omitempty" yaml:"open_with"`
	// Since and Until bound the server versions the metric exists in, Until exclusive. Empty is
	// every version this tool reads. A node outside the range is reported as not having the label,
	// with the reason, rather than as a gap nobody can explain.
	Since string `json:"since,omitempty" yaml:"since,omitempty"`
	Until string `json:"until,omitempty" yaml:"until,omitempty"`
	// Versions is several ranges instead of one, "since-until" with either side open, for a field
	// that came and went within patch releases: "7.0.10-7.0.11", "7.0.16-7.1", "7.3-".
	Versions []string `json:"versions,omitempty" yaml:"versions,omitempty"`
}

// Signal is how a rule's value is made from FTDC paths, once for every sample.
//
// Num and Den are summed lists of paths. "{i}" stands for an instance — a disk, a replica set
// member — and makes the rule run once per instance found. A field MongoDB renamed between versions
// is not an alternative inside one rule: it is a second definition of the same label with its own
// since/until, and the version FTDC records chooses between them.
type Signal struct {
	// Mode is "rate" for counters, read as their change a second, and "gauge" for levels, read as
	// they are.
	Mode string `json:"mode" yaml:"mode"`
	// Unit is what one second's value is in: "%", "ms", "tickets". A time view's label is in
	// seconds a minute; the signal keeps its own unit, which is what the threshold is written in.
	Unit string   `json:"unit" yaml:"unit"`
	Num  []string `json:"num" yaml:"num"`
	Den  []string `json:"den,omitempty" yaml:"den,omitempty"`
	// Minus is subtracted from Num before anything else: the heap outside the cache is what
	// tcmalloc has allocated less what WiredTiger holds.
	Minus []string `json:"minus,omitempty" yaml:"minus,omitempty"`
	// Scale multiplies the result: 100 for a percentage, 0.001 for microseconds into milliseconds.
	Scale float64 `json:"scale,omitempty" yaml:"scale,omitempty"`
	// Complement reports Scale minus the result: memory used from memory available.
	Complement bool `json:"complement,omitempty" yaml:"complement,omitempty"`
	// Behind reads each instance as how far it trails the most advanced one: a replica set
	// member's optime against the newest optime any member reports, in the same sample.
	Behind bool `json:"behind,omitempty" yaml:"behind,omitempty"`
	// MinDen is the least the denominator must sum to over a minute for the minute's ratio to count.
	// A disk that served three requests in a minute has no meaningful time per request.
	MinDen float64 `json:"min_den,omitempty" yaml:"min_den,omitempty"`
	// RealDevice drops instances that are not real block devices.
	RealDevice bool `json:"real_device,omitempty" yaml:"real_device,omitempty"`
}

// The views a rule can be read on.
const (
	ViewMean      = "mean"
	ViewMax       = "max"
	ViewMin       = "min"
	ViewTimeAbove = "time_above"
	ViewTimeBelow = "time_below"
)

// TimeUnit is what a time view's label is in: seconds of each minute spent past the threshold.
const TimeUnit = "s/min"

// Timed reports whether the rule counts seconds past a threshold rather than reading a level.
func (r Rule) Timed() bool { return r.View == ViewTimeAbove || r.View == ViewTimeBelow }

// Unit is what the label's values are in: the signal's unit, or seconds a minute for a time view.
func (r Rule) Unit() string {
	if r.Timed() {
		return TimeUnit
	}
	return r.Signal.Unit
}

// Past reports whether one second's value is past the rule's threshold.
func (r Rule) Past(v float64) bool {
	switch r.View {
	case ViewTimeAbove:
		return v > *r.Threshold
	case ViewTimeBelow:
		return v < *r.Threshold
	}
	return false
}

// Rules is every rule, in file then declaration order, loaded once.
var Rules = mustLoad()

func mustLoad() []Rule {
	rules, err := load()
	if err != nil {
		panic(err)
	}
	return rules
}

func load() ([]Rule, error) {
	entries, err := ruleFiles.ReadDir("rules")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	out := make([]Rule, 0, 64)
	for _, name := range names {
		raw, err := ruleFiles.ReadFile(path.Join("rules", name))
		if err != nil {
			return nil, err
		}
		var pack Pack
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		// A misspelt field is a rule that silently does nothing, so an unknown one is an error.
		decoder.KnownFields(true)
		if err := decoder.Decode(&pack); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, r := range pack.Rules {
			if r.View == "" {
				r.View = ViewMean
			}
			if r.Signal.Scale == 0 {
				r.Signal.Scale = 1
			}
			if err := r.validate(); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", name, r.Name, err)
			}
			for _, earlier := range out {
				if earlier.Name == r.Name && overlap(earlier, r) {
					return nil, fmt.Errorf("%s: %s is defined twice for the same versions; give each definition its own since/until", name, r.Name)
				}
			}
			r.Pack = pack.Pack
			out = append(out, r)
		}
	}
	if err := checkPairs(out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r Rule) validate() error {
	s := r.Signal
	switch {
	case r.Name == "" || r.Title == "" || r.Doc == "":
		return fmt.Errorf("name, title and doc are required")
	case s.Mode != "rate" && s.Mode != "gauge":
		return fmt.Errorf("signal mode %q is neither rate nor gauge", s.Mode)
	case s.Unit == "":
		return fmt.Errorf("the signal needs a unit")
	case len(s.Num) == 0:
		return fmt.Errorf("no numerator")
	case len(r.Levels) == 1:
		return fmt.Errorf("one level is not a scale: give two or more, or none for a shape-only label")
	case len(r.Versions) > 0 && (r.Since != "" || r.Until != ""):
		return fmt.Errorf("give since/until or versions, not both")
	case r.Direction != "" && r.Direction != "up" && r.Direction != "down":
		return fmt.Errorf("direction %q is neither up nor down", r.Direction)
	case s.Behind && (len(s.Num) != 1 || len(s.Den) > 0 || len(s.Minus) > 0 || !strings.Contains(s.Num[0], "{i}")):
		return fmt.Errorf("behind reads one instanced path and nothing else")
	case r.MinChange <= 0:
		return fmt.Errorf("min_change must be positive")
	}
	switch r.View {
	case ViewMean, ViewMax, ViewMin:
		if r.Threshold != nil {
			return fmt.Errorf("a threshold is only read by a time view")
		}
	case ViewTimeAbove, ViewTimeBelow:
		if r.Threshold == nil {
			return fmt.Errorf("view %s needs a threshold", r.View)
		}
	default:
		return fmt.Errorf("view %q is none of mean, max, min, time_above, time_below", r.View)
	}
	for _, v := range r.Versions {
		if !strings.Contains(v, "-") {
			return fmt.Errorf("versions entry %q is not since-until", v)
		}
	}
	for _, rng := range r.ranges() {
		for _, v := range rng {
			if v != "" && parseVersion(v) == nil {
				return fmt.Errorf("version %q is not major.minor[.patch]", v)
			}
		}
	}
	instanced := strings.Contains(s.Num[0], "{i}")
	for _, p := range r.paths() {
		if strings.Contains(p, "|") {
			return fmt.Errorf("%s: alternatives are separate definitions with since/until, not \"|\"", p)
		}
		if strings.Contains(p, "{i}") != instanced {
			return fmt.Errorf("%s: either every path names the instance or none does", p)
		}
	}
	if r.OpenAt != "" && r.levelIndex(r.OpenAt) < 0 {
		return fmt.Errorf("open_at %s is not one of its levels", r.OpenAt)
	}
	return validateLevels(r.Levels)
}

// levelIndex is the position of a band among the rule's levels, -1 when it has none by that name.
func (r Rule) levelIndex(name string) int {
	for i, l := range r.Levels {
		if l.Name == name {
			return i
		}
	}
	return -1
}

// OpenFrom is the index of the lowest band worth opening the label up for, and -1 for a rule
// without levels.
func (r Rule) OpenFrom() int {
	n := len(r.Levels)
	switch {
	case n == 0:
		return -1
	case r.OpenAt != "":
		return r.levelIndex(r.OpenAt)
	case n >= 4:
		return n - 2
	}
	return n - 1
}

// checkPairs refuses a pairs_with that names no label: a pairing that silently finds nothing is a
// drill-down missing the one number that would have explained it.
func checkPairs(rules []Rule) error {
	known := map[string]bool{}
	for _, r := range rules {
		known[r.Name] = true
	}
	for _, r := range rules {
		for _, p := range r.PairsWith {
			if !known[p] {
				return fmt.Errorf("%s: pairs_with %s, which is not a label", r.Name, p)
			}
			if p == r.Name {
				return fmt.Errorf("%s pairs with itself", r.Name)
			}
		}
		for _, p := range r.OpenWith {
			if !known[p] || p == r.Name {
				return fmt.Errorf("%s: open_with %s, which is not another label", r.Name, p)
			}
		}
	}
	return nil
}

// paths is every entry the signal reads.
func (r Rule) paths() []string {
	all := append(append([]string{}, r.Signal.Num...), r.Signal.Den...)
	return append(all, r.Signal.Minus...)
}

// Wants reports whether a path is one some rule reads, so a decode that is choosing what to
// materialise keeps it. The decoder asks this of every column of every chunk, so the answer is
// built once: a set of exact paths and the few instance patterns.
func Wants(p string) bool {
	if wanted.exact[p] {
		return true
	}
	for _, pattern := range wanted.patterns {
		if matches(pattern, p) {
			return true
		}
	}
	return false
}

var wanted = indexWants()

func indexWants() (index struct {
	exact    map[string]bool
	patterns []string
}) {
	index.exact = map[string]bool{}
	for _, r := range Rules {
		for _, entry := range r.paths() {
			if strings.Contains(entry, "{i}") {
				index.patterns = append(index.patterns, entry)
			} else {
				index.exact[entry] = true
			}
		}
	}
	return index
}

// matches compares a rule path, which may hold one "{i}", with a concrete path.
func matches(pattern, p string) bool {
	before, after, instanced := strings.Cut(pattern, "{i}")
	if !instanced {
		return pattern == p
	}
	if !strings.HasPrefix(p, before) || !strings.HasSuffix(p, after) || len(p) <= len(before)+len(after) {
		return false
	}
	return !strings.Contains(p[len(before):len(p)-len(after)], "/")
}

// Definitions is every definition of a label, in file order.
func Definitions(name string) []Rule {
	out := make([]Rule, 0, 2)
	for _, r := range Rules {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

// Names is every label once, in file order.
func Names() []string {
	out, seen := make([]string, 0, len(Rules)), map[string]bool{}
	for _, r := range Rules {
		if !seen[r.Name] {
			seen[r.Name] = true
			out = append(out, r.Name)
		}
	}
	return out
}
