package render

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"mongo-log-parser/internal/series"
)

type GnuplotOptions struct {
	Width, Height int
	Overlay, Grid bool
	Title         string
	First, Last   float64 // UTC epoch milliseconds; zero uses the series coverage
}

// GnuplotReport uses the monochrome dumb terminal. Data is sent through stdin, never a shell.
// It leaves statistics and buckets untouched and uses their declared plot aggregation.
func GnuplotReport(rows []series.Series, options GnuplotOptions) (string, error) {
	if len(rows) == 0 {
		return "No metrics to show.", nil
	}
	if _, err := exec.LookPath("gnuplot"); err != nil {
		return "", fmt.Errorf("gnuplot is not installed; install it with brew install gnuplot (macOS) or sudo apt install gnuplot (Debian/Ubuntu)")
	}
	groups := gnuplotGroups(rows, options)
	first, last := options.First, options.Last
	if !(last > first) {
		first, last = overlaySpan(rows)
	}
	if !(last > first) {
		last = first + float64(max(1, rows[0].BucketSeconds))*1000
	}
	scales := sharedScales(rows)
	var script strings.Builder
	width, height := max(60, options.Width), max(18, options.Height+10)
	fmt.Fprintf(&script, "set terminal dumb size %d,%d noenhanced\nset encoding utf8\nset xdata time\nset timefmt '%%s'\nset datafile missing '?'\nset border 3\nset tics nomirror\nset key outside bottom center\nset xlabel 'UTC'\nset xrange [%.3f:%.3f]\n", width, height, first/1000, last/1000)
	if last-first >= 24*60*60*1000 {
		script.WriteString("set format x '%m-%d %H:%M'\n")
	} else {
		script.WriteString("set format x '%H:%M'\n")
	}
	columns := 1
	if options.Grid {
		columns = min(GridPanels(width), max(1, len(sourcesOf(rows))))
		fmt.Fprintf(&script, "set terminal dumb size %d,%d noenhanced\nset multiplot layout %d,%d rowsfirst\n", width, height*((len(groups)+columns-1)/columns), (len(groups)+columns-1)/columns, columns)
	}
	anyData := false
	for _, group := range groups {
		active := make([]series.Series, 0, len(group))
		for _, row := range group {
			if len(row.Buckets) > 0 {
				active = append(active, row)
			}
		}
		if len(active) == 0 {
			continue
		}
		anyData = true
		title := options.Title
		if title == "" {
			title = nameOf(active[0])
		}
		title += " | " + binNote(active)
		fmt.Fprintf(&script, "set title %s\nset ylabel %s\n", gnuplotString(title), gnuplotString(active[0].Unit))
		lo, hi := scales[active[0].Metric][0], scales[active[0].Metric][1]
		if options.Title != "" {
			lo, hi = overlayScale(active)
		}
		if math.IsInf(lo, 0) || math.IsInf(hi, 0) || math.IsNaN(lo) || math.IsNaN(hi) {
			lo, hi = 0, 1
		}
		if hi == lo {
			padding := math.Max(1, math.Abs(hi)*0.1)
			lo -= padding
			hi += padding
		} else {
			// Keep zeros and extrema off the frame so border glyphs cannot hide them.
			padding := (hi - lo) * 0.05
			lo -= padding
			hi += padding
		}
		fmt.Fprintf(&script, "set yrange [%.17g:%.17g]\nplot ", lo, hi)
		for i, row := range active {
			if i > 0 {
				script.WriteString(", ")
			}
			legend := row.Source
			if options.Title != "" {
				legend = nameOf(row)
			} else if len(active) == 1 {
				legend += " | " + BinLabel(row.BucketSeconds) + "/bin"
			}
			legend += fmt.Sprintf(" | n=%d", row.SampleCount)
			marks := "*#+xo@%=ABCDEFGH"
			fmt.Fprintf(&script, "'-' using 1:2 with linespoints lt %d pt %s title %s", i+1, gnuplotString(string(marks[i%len(marks)])), gnuplotString(legend))
		}
		script.WriteByte('\n')
		for _, row := range active {
			script.WriteString(gnuplotData(row))
			script.WriteString("e\n")
		}
	}
	if options.Grid {
		script.WriteString("unset multiplot\n")
	}
	if !anyData {
		return "No samples in this window.\n" + LineTable(rows, TableOptions{Width: width}), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gnuplot", "-d")
	cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	cmd.Stdin = strings.NewReader(script.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gnuplot failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	summary := append([]series.Series(nil), rows...)
	for i := range summary {
		if summary[i].Source != "" {
			summary[i].Label = summary[i].Source + " / " + nameOf(summary[i])
		}
	}
	headerFields := reportHeader(rows, first, last)
	for i := range headerFields {
		if headerFields[i] == ". = gap" {
			headerFields[i] = "gaps = missing samples"
		}
	}
	header := strings.Join(headerFields, " | ")
	return header + "\n" + strings.TrimRight(stdout.String(), "\n") + "\n\n" + LineTable(summary, TableOptions{Width: width}), nil
}

func gnuplotGroups(rows []series.Series, options GnuplotOptions) [][]series.Series {
	if options.Title != "" {
		return [][]series.Series{rows}
	}
	groups := make([][]series.Series, 0, len(rows))
	indices := map[string]int{}
	for _, row := range rows {
		if options.Overlay {
			if index, ok := indices[row.Metric]; ok {
				groups[index] = append(groups[index], row)
				continue
			}
			indices[row.Metric] = len(groups)
		}
		groups = append(groups, []series.Series{row})
	}
	return groups
}

// Single-quoted strings disable gnuplot's backquote shell substitution. Doubled quotes keep
// names such as "x'; system 'command'" inside the literal rather than executing them.
func gnuplotString(text string) string {
	text = strings.NewReplacer("'", "''", "\n", " ", "\r", " ", "\t", " ").Replace(text)
	return "'" + text + "'"
}

func gnuplotData(row series.Series) string {
	var data strings.Builder
	var previous float64
	for i, bucket := range row.Buckets {
		stamp := bucket.StartMillis() / 1000
		if i > 0 && stamp-previous > float64(max(1, row.BucketSeconds)) {
			fmt.Fprintf(&data, "%.3f ?\n", previous+float64(max(1, row.BucketSeconds)))
		}
		value := bucket.Max
		if row.Low {
			value = bucket.Min
		} else if row.PlotAggregation == "avg" {
			value = bucket.Avg
		}
		number := "?"
		if bucket.Samples > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) {
			number = strconv.FormatFloat(value, 'g', 17, 64)
		}
		fmt.Fprintf(&data, "%.3f %s\n", stamp, number)
		previous = stamp
	}
	return data.String()
}
