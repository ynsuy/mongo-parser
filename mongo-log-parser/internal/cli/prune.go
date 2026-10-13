package cli

import (
	"fmt"
	"os"
	"time"

	"mongo-log-parser/internal/render"
	"mongo-log-parser/internal/store"
)

// prune is the one command that is about this machine rather than about a bundle: a store is kept
// so the next question is instant, which means the cache only ever grows. Listing is the default
// and removing has to be asked for, because the cost of keeping a store is disk and the cost of
// removing one somebody still wants is reading a bundle again.

type prunedStore struct {
	Bundle   string `json:"bundle"`
	Dir      string `json:"store"`
	Bytes    int64  `json:"bytes"`
	Built    string `json:"built"`
	LastUsed string `json:"last_used"`
	Removed  bool   `json:"removed"`
	Missing  bool   `json:"bundle_missing"`
}

func runPrune(_ string, args []string) error {
	fs, c := newFlags("prune")
	olderThan := fs.Duration("older-than", 0, "remove the stores nothing has read for this long, e.g. 720h")
	all := fs.Bool("all", false, "remove every store")
	gone := fs.Bool("gone", false, "remove the stores whose bundle is no longer on this disk")
	dryRun := fs.Bool("dry-run", false, "say what would be removed and remove nothing")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cached, err := store.Cache()
	if err != nil {
		return err
	}
	if len(cached) == 0 {
		return emit(os.Stdout, c.json, "prune", store.CacheRoot(), []prunedStore{},
			withVersion("no stores here yet: "+store.CacheRoot()))
	}

	rows := make([]prunedStore, 0, len(cached))
	var total, freed int64
	for _, item := range cached {
		row := prunedStore{Bundle: item.Bundle, Dir: item.Dir, Bytes: item.Bytes,
			Built: stamp2(item.Built), LastUsed: stamp2(item.LastUsed)}
		if row.Bundle == "" {
			row.Bundle = "(not recorded)"
		} else if _, err := os.Stat(row.Bundle); err != nil {
			row.Missing = true
		}
		total += item.Bytes

		wanted := *all ||
			(*olderThan > 0 && !item.LastUsed.IsZero() && time.Since(item.LastUsed) > *olderThan) ||
			(*gone && row.Missing)
		if wanted {
			if !*dryRun {
				if err := store.Remove(item.Dir); err != nil {
					return fmt.Errorf("removing %s: %w", item.Dir, err)
				}
			}
			row.Removed = true
			freed += item.Bytes
		}
		rows = append(rows, row)
	}

	options := render.TableOptions{Width: outputWidth(c.width), Color: useColor("never") && !c.json}
	table := render.Table([]render.Column{
		{Title: "Bundle", Values: pick(rows, func(r prunedStore) string {
			if r.Missing {
				return r.Bundle + " (gone)"
			}
			return r.Bundle
		})},
		{Title: "Size", Align: render.AlignRight, Values: pick(rows, func(r prunedStore) string { return bytesText(r.Bytes) })},
		{Title: "Read", Values: pick(rows, func(r prunedStore) string { return r.Built })},
		{Title: "Last used", Values: pick(rows, func(r prunedStore) string { return r.LastUsed })},
		{Title: "", Values: pick(rows, func(r prunedStore) string {
			switch {
			case r.Removed && *dryRun:
				return "would remove"
			case r.Removed:
				return "removed"
			}
			return ""
		})},
	}, options)

	var note string
	switch {
	case freed > 0 && *dryRun:
		note = fmt.Sprintf("%s in %s would be freed; run the same command without --dry-run", bytesText(freed), plural(countRemoved(rows), "store"))
	case freed > 0:
		note = fmt.Sprintf("%s freed from %s", bytesText(freed), plural(countRemoved(rows), "store"))
	default:
		note = fmt.Sprintf("%s in %s under %s\nnothing was removed: --older-than 720h, --gone or --all says what to remove, --dry-run shows it first",
			bytesText(total), plural(len(rows), "store"), store.CacheRoot())
	}
	return emit(os.Stdout, c.json, "prune", store.CacheRoot(), rows, withVersion(table+"\n\n"+note))
}

func countRemoved(rows []prunedStore) int {
	n := 0
	for _, row := range rows {
		if row.Removed {
			n++
		}
	}
	return n
}

func pick(rows []prunedStore, read func(prunedStore) string) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = read(row)
	}
	return out
}

// stamp2 is a date a person reads, or a dash when nothing recorded one.
func stamp2(at time.Time) string {
	if at.IsZero() {
		return "-"
	}
	since := time.Since(at)
	switch {
	case since < 24*time.Hour:
		return at.Local().Format("15:04")
	case since < 365*24*time.Hour:
		return at.Local().Format("01-02 15:04")
	}
	return at.Local().Format("2006-01-02")
}
