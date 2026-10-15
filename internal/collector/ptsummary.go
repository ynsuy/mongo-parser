// Package collector reads the output a Percona diagnostic collector leaves beside a bundle's logs.
//
// It is the last place to ask how much data there is. An initial sync's statistics say it exactly
// and are rarely there; diagnostic data says what the filesystems hold and only from 8.0; and
// before that the one thing in a bundle that knows is a pt-summary one of the collectors wrote.
//
// Nothing here opens a file. The bundle is read once, at ingest, and a collector's output is kept
// whole in the store; this parses the text the caller hands it, so the same summary is never read
// off disk twice and a pt-summary inside an archive is as readable as one beside the logs.
package collector

import (
	"bufio"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Filesystem is one mounted filesystem as pt-summary found it.
type Filesystem struct {
	Device    string  `json:"device"`
	Mount     string  `json:"mount"`
	Type      string  `json:"type"`
	Capacity  float64 `json:"capacity_bytes"`
	UsedShare float64 `json:"used_share"`
	UsedBytes float64 `json:"used_bytes"`
	// Source is the pt-summary this came from, so a figure can be traced to a file.
	Source string `json:"source"`
}

// Summary is what one pt-summary says, of the parts worth reading here.
type Summary struct {
	Source      string       `json:"source"`
	Host        string       `json:"host,omitempty"`
	Filesystems []Filesystem `json:"filesystems,omitempty"`
}

// ErrNotSummary is a text that never named itself as a pt-summary. The file name is a weak test —
// a bundle can hold anything under any name — so nothing is believed until the report says what it
// is.
var ErrNotSummary = errors.New("not a Percona Toolkit system summary")

// Summaries parses the collector outputs a bundle holds, keyed by where each one came from.
//
// A collector writes one per host and a bundle carries one per node, so the figures are per machine
// and are labelled with the host the report itself names rather than with the path, which is
// whatever the person collecting them called it. Anything that is not a pt-summary is skipped
// rather than refused: the same table holds every other artifact the bundle carried.
func Summaries(texts map[string]string) []Summary {
	found := make([]Summary, 0, len(texts))
	for source, text := range texts {
		summary, err := Parse(source, text)
		if err != nil {
			continue
		}
		found = append(found, summary)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Source < found[j].Source })
	return found
}

// Parse reads the sections this package uses, and refuses a text that is not a pt-summary.
func Parse(source, text string) (Summary, error) {
	summary := Summary{Source: source}
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	section, confirmed := "", false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			// The line that names the tool is itself a section heading — "# Percona Toolkit System
			// Summary Report ####" — so the check has to happen here rather than after it.
			if strings.Contains(line, "Percona Toolkit System Summary Report") {
				confirmed = true
			}
			section = strings.ToLower(strings.Trim(line, "# "))
			continue
		}
		if !confirmed {
			continue
		}
		switch {
		case strings.HasPrefix(section, "mounted filesystems"):
			if filesystem, ok := readFilesystem(line); ok {
				filesystem.Source = source
				summary.Filesystems = append(summary.Filesystems, filesystem)
			}
		case summary.Host == "":
			if host, ok := strings.CutPrefix(strings.TrimSpace(line), "Hostname |"); ok {
				summary.Host = strings.TrimSpace(host)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Summary{}, err
	}
	if !confirmed {
		return Summary{}, ErrNotSummary
	}
	return summary, nil
}

// readFilesystem reads one row of the mounted filesystems table.
//
// The columns are the device, the size, the percentage used, the type, the mount options and the
// mount point. The options are one comma-separated token and the mount point is last, which is what
// makes the row readable by position at all — and a row whose size does not parse is the header.
func readFilesystem(line string) (Filesystem, bool) {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return Filesystem{}, false
	}
	capacity, ok := bytesOf(fields[1])
	if !ok {
		return Filesystem{}, false
	}
	share, ok := shareOf(fields[2])
	if !ok {
		return Filesystem{}, false
	}
	return Filesystem{
		Device: fields[0], Mount: fields[len(fields)-1], Type: fields[3],
		Capacity: capacity, UsedShare: share, UsedBytes: capacity * share,
	}, true
}

// bytesOf reads pt-summary's sizes, which are rounded to one decimal and suffixed: 1.5T, 20G, 100G.
// They are powers of 1024, the way df -h writes them.
func bytesOf(text string) (float64, bool) {
	if text == "" {
		return 0, false
	}
	scale := 1.0
	switch text[len(text)-1] {
	case 'K', 'k':
		scale = 1 << 10
	case 'M', 'm':
		scale = 1 << 20
	case 'G', 'g':
		scale = 1 << 30
	case 'T', 't':
		scale = 1 << 40
	case 'P', 'p':
		scale = 1 << 50
	default:
		// No suffix: the header's "Size", or a figure in bytes.
		value, err := strconv.ParseFloat(text, 64)
		return value, err == nil
	}
	value, err := strconv.ParseFloat(text[:len(text)-1], 64)
	return value * scale, err == nil
}

// shareOf reads "57%" as 0.57.
func shareOf(text string) (float64, bool) {
	trimmed, ok := strings.CutSuffix(text, "%")
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	return value / 100, err == nil
}
