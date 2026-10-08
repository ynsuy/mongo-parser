package label

import (
	"fmt"
	"strings"
)

// parseVersion reads the leading numbers of a version string: "7.0.14-8" is [7 0 14]. Nil when it
// does not start with a number.
func parseVersion(v string) []int {
	out := make([]int, 0, 3)
	for _, part := range strings.SplitN(v, ".", 3) {
		n, digits := 0, 0
		for _, r := range part {
			if r < '0' || r > '9' {
				break
			}
			n, digits = n*10+int(r-'0'), digits+1
		}
		if digits == 0 {
			break
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// before reports whether version a is older than b, comparing as far as both go.
func before(a, b []int) bool {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// ranges is every [since, until) range a definition covers.
func (r Rule) ranges() [][2]string {
	if len(r.Versions) == 0 {
		return [][2]string{{r.Since, r.Until}}
	}
	out := make([][2]string, 0, len(r.Versions))
	for _, v := range r.Versions {
		since, until, _ := strings.Cut(v, "-")
		out = append(out, [2]string{since, until})
	}
	return out
}

// inRange reports whether a version falls in [since, until).
func inRange(v []int, r [2]string) bool {
	if since := parseVersion(r[0]); since != nil && before(v, since) {
		return false
	}
	if until := parseVersion(r[1]); until != nil && !before(v, until) {
		return false
	}
	return true
}

// applies reports whether a node running version may carry the rule's metrics, and if not, why. An
// unknown version applies: the metrics themselves will say whether they are there.
func (r Rule) applies(version string) (bool, string) {
	v := parseVersion(version)
	if v == nil {
		return true, ""
	}
	for _, rng := range r.ranges() {
		if inRange(v, rng) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("defined for %s; this node runs %s", r.VersionText(), version)
}

// VersionText is a definition's versions in words.
func (r Rule) VersionText() string {
	parts := make([]string, 0, 2)
	for _, rng := range r.ranges() {
		switch {
		case rng[0] != "" && rng[1] != "":
			parts = append(parts, rng[0]+" up to "+rng[1])
		case rng[0] != "":
			parts = append(parts, rng[0]+" and later")
		case rng[1] != "":
			parts = append(parts, "before "+rng[1])
		default:
			parts = append(parts, "all versions")
		}
	}
	return strings.Join(parts, ", ")
}

// overlap reports whether two definitions could both apply to one version. An open end reaches
// every version on that side.
func overlap(a, b Rule) bool {
	// p ends before q starts.
	ends := func(p, q [2]string) bool {
		until, since := parseVersion(p[1]), parseVersion(q[0])
		return until != nil && since != nil && !before(since, until)
	}
	for _, x := range a.ranges() {
		for _, y := range b.ranges() {
			if !ends(x, y) && !ends(y, x) {
				return true
			}
		}
	}
	return false
}
