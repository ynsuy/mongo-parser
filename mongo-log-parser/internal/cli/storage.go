package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"mongo-log-parser/internal/analyze"
	"mongo-log-parser/internal/render"
	"mongo-log-parser/internal/series"
)

// runStorage answers "how much data is there", which a bundle usually cannot answer at all.
//
// A MongoDB log states the size of the data in exactly one line, and only when a member happened to
// be rebuilding from a peer while the logs were collected: the initial sync statistics, which carry
// the deployment's total and then every collection the clone had reached, with its documents, its
// bytes and how many indexes it has to rebuild. Nothing else in a log says any of it.
func runStorage(root string, args []string) error {
	flags, c := newFlags("storage")
	w := addWindowFlags(flags)
	top := flags.Int("top", 20, "how many collections to show")
	color := flags.String("color", "never", "never (default), auto or always")
	if err := parseOnly(flags, args); err != nil {
		return err
	}
	if err := bounded(bound{"top", *top, 1}); err != nil {
		return err
	}
	started := time.Now()
	st, err := openStore(root, c.refresh)
	if err != nil {
		return err
	}
	defer st.Close()
	start, end, err := w.resolve(st, time.Time{})
	if err != nil {
		return err
	}
	q, err := readQuality(st)
	if err != nil {
		return err
	}

	rebuilds := analyze.NewRebuilds()
	if err := analyze.Run(st, windowQuery(start, end), rebuilds); err != nil {
		return err
	}
	sizes := dataSizes(st)

	options := render.TableOptions{Width: outputWidth(c.width), Color: useColor(*color) && !c.json}
	rendered := renderStorage(rebuilds, sizes, *top, options)
	footer := fmt.Sprintf("%sthe bundle holds %s in %s; answered in %s",
		askedWindow(start, end),
		plural(q.Parsed, "record"), plural(q.Files, "log stream"),
		time.Since(started).Round(time.Millisecond))
	return emit(os.Stdout, c.json, "storage", root, map[string]any{
		"rebuilds": rebuilds.All(), "sizes": sizes, "quality": q,
	}, withVersion(rendered+"\n\n"+footer))
}

// renderStorage answers how much data there is from every source that can, most trustworthy first.
//
// They are not the same number and they are never added or substituted: an initial sync states the
// logical size of the documents, a filesystem states what WiredTiger wrote after compressing them,
// and the oplog is neither. Each line says which it is, and where none of them can answer the
// section says that rather than reaching for the nearest number.
func renderStorage(rebuilds *analyze.Rebuilds, sizes []DataSize, limit int, options render.TableOptions) string {
	rebuild, found := rebuilds.Largest()
	if !found {
		if line := sizeLines(sizes); line != "" {
			return "no initial sync statistics in this window, so the logical size of the data and its breakdown by\n" +
				"collection are not in this bundle — a member has to be rebuilding for a log to state them.\n" +
				"What the rest of the bundle does say:\n\n" + line
		}
		return "no initial sync statistics in this window, and nothing else in a MongoDB log states how much data there is:\n" +
			"a member has to be rebuilding from a peer while the logs are collected for the question to have an answer here.\n" +
			"The diagnostic data carries the oplog's size from 7.0 and the filesystems from 8.0, and a collector's pt-summary\n" +
			"carries them further back; this bundle holds none of the three."
	}

	parts := []string{rebuildSummary(rebuild)}
	if line := sizeLines(sizes); line != "" {
		// Beneath the logical figure and labelled apart from it: the two differ by the compression
		// ratio and a reader who takes one for the other is out by three or four times.
		parts = append(parts, "and what the rest of the bundle says, which is a different measurement:\n"+line)
	}
	if table := renderClonedCollections(rebuild, limit, options); table != "" {
		parts = append(parts, table)
	}
	if table := renderSyncAttempts(rebuild, options); table != "" {
		parts = append(parts, "initial sync attempts\n"+table)
	}
	return joinSections(parts)
}

// rebuildSummary is the headline: how much data, how far the copy got, and whether the figures
// below are all of them.
func rebuildSummary(rebuild analyze.Rebuild) string {
	line := fmt.Sprintf("%s of data on %s, as %s reported it",
		render.Amount(rebuild.TotalBytes, series.UnitBytes),
		plural(rebuild.DatabasesToClone, "database"), rebuild.Source)
	if rebuild.Status != "" {
		line += fmt.Sprintf("\nthe rebuild was %s", strings.ReplaceAll(rebuild.Status, "_", " "))
		if rebuild.TotalBytes > 0 {
			line += fmt.Sprintf(", %.0f%% copied (%s of %s)", rebuild.Progress()*100,
				render.Amount(rebuild.BytesCopied, series.UnitBytes),
				render.Amount(rebuild.TotalBytes, series.UnitBytes))
		}
		if rebuild.ElapsedMillis > 0 {
			line += fmt.Sprintf(", running for %s", render.Duration(time.Duration(rebuild.ElapsedMillis)*time.Millisecond))
		}
	}
	if rebuild.FailedAttempts > 0 {
		line += fmt.Sprintf("\n%d of %d permitted attempts had already failed",
			rebuild.FailedAttempts, rebuild.MaxAttempts)
		if reason := analyze.Reason(rebuild.LastFailure()); reason != "" {
			line += ", the last of them on: " + reason
		}
	}
	// Said before the table rather than under it, because it decides how the table may be read.
	if rebuild.Truncated {
		line += fmt.Sprintf("\nthe server cut this line short, dropping %d entries: the total above is its own figure and is complete,"+
			"\nthe collections below are the ones that fitted in the line and not the ones there are", rebuild.Omitted)
	}
	return line
}

// renderClonedCollections is the inventory: which collections hold the data.
func renderClonedCollections(rebuild analyze.Rebuild, limit int, options render.TableOptions) string {
	if len(rebuild.Collections) == 0 {
		return ""
	}
	shown := rebuild.Collections[:min(limit, len(rebuild.Collections))]
	text := func(read func(analyze.ClonedCollection) string) []string {
		values := make([]string, len(shown))
		for i, collection := range shown {
			values[i] = read(collection)
		}
		return values
	}
	table := render.Table([]render.Column{
		{Title: "Namespace", Align: render.AlignTail, Values: text(func(c analyze.ClonedCollection) string {
			return c.Namespace
		})},
		{Title: "Size", Align: render.AlignRight, Values: text(func(c analyze.ClonedCollection) string {
			return render.Amount(c.Bytes, series.UnitBytes)
		})},
		{Title: "Share", Align: render.AlignRight, Values: text(func(c analyze.ClonedCollection) string {
			if rebuild.TotalBytes <= 0 {
				return "-"
			}
			return fmt.Sprintf("%.1f%%", c.Bytes/rebuild.TotalBytes*100)
		}), Drop: 3},
		{Title: "Documents", Align: render.AlignRight, Values: text(func(c analyze.ClonedCollection) string {
			return render.Compact(c.Documents)
		})},
		{Title: "Avg doc", Align: render.AlignRight, Values: text(func(c analyze.ClonedCollection) string {
			if c.Documents <= 0 {
				return "-"
			}
			return render.Amount(c.Bytes/c.Documents, series.UnitBytes)
		}), Drop: 2},
		// The index count is what a rebuild actually costs: every one of them is built again on the
		// node being seeded, after the documents land.
		{Title: "Indexes", Align: render.AlignRight, Values: text(func(c analyze.ClonedCollection) string {
			return strconv.Itoa(c.Indexes)
		})},
		{Title: "Cloned in", Align: render.AlignRight, Values: text(func(c analyze.ClonedCollection) string {
			if c.Millis <= 0 {
				return "-"
			}
			return render.Duration(time.Duration(c.Millis) * time.Millisecond)
		}), Drop: 1},
		// How far this collection's copy got, rather than whether it finished. The finished flag is
		// an end stamp, and a truncated line is exactly where the server drops one — so a
		// collection that was copied would read as still running because its stamp did not fit.
		// The copy counters are always there.
		{Title: "Copied", Align: render.AlignRight, Values: text(func(c analyze.ClonedCollection) string {
			if c.Bytes <= 0 {
				return "-"
			}
			return fmt.Sprintf("%.0f%%", min(c.Copied/c.Bytes, 1)*100)
		})},
	}, options)
	note := fmt.Sprintf("%s listed, %s cloned of %s",
		plural(len(rebuild.Collections), "collection"),
		strconv.Itoa(rebuild.DatabasesCloned), plural(rebuild.DatabasesToClone, "database"))
	return table + "\n" + note
}

// renderSyncAttempts lists what each try at the rebuild died of, which is the question a node on
// its latest of several attempts raises.
func renderSyncAttempts(rebuild analyze.Rebuild, options render.TableOptions) string {
	if len(rebuild.Attempts) == 0 {
		return ""
	}
	text := func(read func(int, analyze.SyncAttempt) string) []string {
		values := make([]string, len(rebuild.Attempts))
		for i, attempt := range rebuild.Attempts {
			values[i] = read(i, attempt)
		}
		return values
	}
	return render.Table([]render.Column{
		{Title: "#", Align: render.AlignRight, Values: text(func(i int, _ analyze.SyncAttempt) string {
			return strconv.Itoa(i + 1)
		})},
		{Title: "Ran for", Align: render.AlignRight, Values: text(func(_ int, a analyze.SyncAttempt) string {
			return render.Duration(time.Duration(a.Millis) * time.Millisecond)
		})},
		{Title: "Source", Align: render.AlignTail, Values: text(func(_ int, a analyze.SyncAttempt) string {
			return orDash(a.SyncSource)
		}), Drop: 2},
		{Title: "Unreachable", Align: render.AlignRight, Values: text(func(_ int, a analyze.SyncAttempt) string {
			return render.Duration(time.Duration(a.Unreachable) * time.Millisecond)
		}), Drop: 1},
		{Title: "Ended on", Elastic: true, Values: text(func(_ int, a analyze.SyncAttempt) string {
			if !a.Failed() {
				return "completed"
			}
			return analyze.Reason(a.Status)
		})},
	}, options)
}

// joinSections puts a blank line between the parts of a section.
func joinSections(parts []string) string {
	out := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		if out != "" {
			out += "\n\n"
		}
		out += part
	}
	return out
}

// sizeLines is what the rest of the bundle says, in the order it can be trusted.
func sizeLines(sizes []DataSize) string {
	lines := make([]string, 0, 4)
	if mount, ok := dataMount(sizes); ok {
		lines = append(lines,
			fmt.Sprintf("  on disk  %s of %s used on %s, the largest filesystem %s has",
				render.Amount(mount.MountUsed, series.UnitBytes),
				render.Amount(mount.MountCapacity, series.UnitBytes),
				mount.Mount, mount.Source),
			// Under the number rather than beside it, because it is what the number does not say.
			"           "+mountCaveat(mount))
	}
	for _, size := range sizes {
		if size.Kind != SizeOplog {
			continue
		}
		line := fmt.Sprintf("  oplog    %s of the %s configured",
			render.Amount(size.OplogBytes, series.UnitBytes),
			render.Amount(size.OplogMaxBytes, series.UnitBytes))
		if size.OplogOnDisk > 0 {
			line += fmt.Sprintf(", %s on disk", render.Amount(size.OplogOnDisk, series.UnitBytes))
		}
		if size.OplogDocuments > 0 {
			line += fmt.Sprintf(", %s entries", render.Compact(size.OplogDocuments))
		}
		lines = append(lines, line+", on "+size.Source)
	}
	return strings.Join(lines, "\n")
}

// hasMount reports whether a filesystem figure is already among these.
func hasMount(sizes []DataSize) bool {
	for _, size := range sizes {
		if size.Kind == SizeMount {
			return true
		}
	}
	return false
}

// mountCaveat is what a filesystem figure does not say, which is most of what a reader wants to
// know about it.
func mountCaveat(mount DataSize) string {
	caveat := "the whole mount after compression, not MongoDB's share of it"
	if mount.Rounded {
		// pt-summary writes "1.5T" and "50%", so the bytes are that arithmetic and not a reading.
		caveat += "; a collector's report, rounded before it was written"
	}
	return caveat
}
