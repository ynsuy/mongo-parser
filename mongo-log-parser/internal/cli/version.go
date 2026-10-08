package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// A bundle report is read away from the machine that made it, and acted on. "Which parser said
// this" therefore has to be answerable from the output itself: a tool that is rebuilt from a
// working tree on every run can be any age at all, and nothing in a table of numbers says which.
//
// Version and BuiltAt are stamped at link time:
//
//	go build -ldflags "-X mongo-log-parser/internal/cli.Version=0.1.0+abc123 \
//	                   -X mongo-log-parser/internal/cli.BuiltAt=2026-09-15T10:00:00Z"
//
// scripts/version.sh derives the string; an unstamped build says "dev" rather than claiming a
// number it does not have.
var (
	Version = "dev"
	BuiltAt = ""
)

// Stamped reports whether this binary knows what it is. An unstamped one is a working-tree build,
// which is a fact worth printing rather than hiding behind a plausible-looking default.
func Stamped() bool { return Version != "dev" && Version != "" }

// Release is the short form that goes in a report footer: enough to tell two builds apart, short
// enough that it does not push the line that matters off the screen.
func Release() string {
	if !Stamped() {
		return "dev build"
	}
	return Version
}

// versionLine is the whole answer, for `mongo-log-parser --version`.
func versionLine() string {
	parts := []string{"mongo-log-parser " + Release()}
	if BuiltAt != "" {
		parts = append(parts, "built "+BuiltAt)
	}
	parts = append(parts, runtime.Version()+" "+runtime.GOOS+"/"+runtime.GOARCH)
	// A module built outside a module cache has no build info to add; this is extra, never the
	// only thing printed.
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				parts = append(parts, "revision "+setting.Value)
			}
		}
	}
	line := strings.Join(parts, ", ")
	if !Stamped() {
		line += "\nThis binary was not stamped with a version. It is whatever the working tree held when it was built:\nbuild it through scripts/version.sh to get an identifiable one."
	}
	return line
}

// versionNote is the footer line every command ends with, so a report pasted into a ticket carries
// the tool that produced it.
func versionNote() string {
	if BuiltAt == "" {
		return fmt.Sprintf("mongo-log-parser %s", Release())
	}
	return fmt.Sprintf("mongo-log-parser %s, built %s", Release(), BuiltAt)
}

// withVersion is how every command's text ends: the answer, then the build that produced it. A
// report is read away from the machine that made it and acted on, and "which parser said this"
// cannot be reconstructed afterwards from a table of numbers.
func withVersion(text string) string {
	return strings.TrimRight(text, "\n") + "\n\n" + versionNote()
}
