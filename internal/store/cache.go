package store

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A store is kept because the whole point of one is to still be there when the next question is
// asked — a week later, when the ticket comes back. That makes the cache grow, so it is something a
// person has to be able to see and clear, and Tidy clears what is unlikely to be asked again.

// usedMarker is touched every time a store is opened. The store itself is opened read-only and its
// file's modification time says when the bundle was read, not when anyone last looked at it; after
// a month of stores, the second is the one worth keeping.
const usedMarker = ".used"

// Cached is one bundle's store on disk.
type Cached struct {
	Dir      string    // where it lives
	Bundle   string    // the bundle it was built from, as recorded when it was read
	Bytes    int64     // what it costs
	Built    time.Time // when the bundle was read into it
	LastUsed time.Time // when a command last answered from it
}

// markUsed records that a store was opened now.
func markUsed(dir string) {
	path := filepath.Join(dir, usedMarker)
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return
	}
	if file, err := os.Create(path); err == nil {
		file.Close()
	}
}

// CacheRoot is the directory every store lives under.
func CacheRoot() string { return filepath.Dir(Dir("x")) }

// Cache lists the stores on this machine, the largest first.
func Cache() ([]Cached, error) {
	root := CacheRoot()
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]Cached, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		info, err := os.Stat(filepath.Join(dir, "store.db"))
		if err != nil {
			// A directory with no store in it is an ingest that was interrupted; it is still disk
			// somebody is paying for, so it is listed rather than hidden.
			item := Cached{Dir: dir, Bundle: interrupted, Bytes: dirBytes(dir)}
			if info, err := entry.Info(); err == nil {
				item.LastUsed = info.ModTime()
			}
			out = append(out, item)
			continue
		}
		item := Cached{Dir: dir, Bytes: dirBytes(dir), Built: info.ModTime(), LastUsed: info.ModTime()}
		if used, err := os.Stat(filepath.Join(dir, usedMarker)); err == nil && used.ModTime().After(item.LastUsed) {
			item.LastUsed = used.ModTime()
		}
		item.Bundle = bundleOf(dir)
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, nil
}

// bundleOf reads back which bundle a store was built from, so a listing names paths rather than
// the hashed directories they are keyed by.
func bundleOf(dir string) string {
	db, err := open(filepath.Join(dir, "store.db") + "?mode=ro")
	if err != nil {
		return ""
	}
	defer db.Close()
	var bundle string
	_ = db.QueryRow(`SELECT value FROM meta WHERE key='root'`).Scan(&bundle)
	return bundle
}

func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			if info, err := entry.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// bundleFile names the bundle a store was built from in plain text, so a hook deciding whether a
// path is a bundle can list them without opening every store.
const bundleFile = "bundle"

// Remember records which bundle the store at dir answers for.
func Remember(dir, root string) {
	path := filepath.Join(dir, bundleFile)
	if old, err := os.ReadFile(path); err == nil && string(old) == root {
		return
	}
	_ = os.WriteFile(path, []byte(root), 0o644)
}

// Bundles is every bundle this machine has a store for.
func Bundles() []string {
	dirs, _ := filepath.Glob(filepath.Join(CacheRoot(), "*", bundleFile))
	out := make([]string, 0, len(dirs))
	for _, path := range dirs {
		if root, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(root)) != "" {
			out = append(out, strings.TrimSpace(string(root)))
		}
	}
	return out
}

const interrupted = "(interrupted read)"

const (
	defaultKeepDays = 14
	defaultCacheGB  = 20
	// A store used this recently may be open in another session.
	inUse = time.Hour
)

// Tidy runs after a bundle is read into a new store at keep, because a large bundle's store runs
// to gigabytes and nothing else removes one. It drops the stores unused for MONGO_PARSER_KEEP_DAYS
// and those whose bundle is gone from this disk, then the least recently used until the cache fits
// in MONGO_PARSER_CACHE_GB. 0 turns either off.
func Tidy(keep string) ([]Cached, error) {
	items, err := Cache()
	if err != nil {
		return nil, err
	}
	maxAge := time.Duration(envInt("MONGO_PARSER_KEEP_DAYS", defaultKeepDays)) * 24 * time.Hour
	maxBytes := int64(envInt("MONGO_PARSER_CACHE_GB", defaultCacheGB)) << 30
	exists := func(path string) bool { _, err := os.Stat(path); return err == nil }
	var removed []Cached
	for _, item := range expired(items, keep, time.Now(), maxAge, maxBytes, exists) {
		if err := Remove(item.Dir); err != nil {
			return removed, err
		}
		removed = append(removed, item)
	}
	return removed, nil
}

func expired(items []Cached, keep string, now time.Time, maxAge time.Duration, maxBytes int64, exists func(string) bool) []Cached {
	var out, left []Cached
	var total int64
	for _, item := range items {
		idle := now.Sub(item.LastUsed)
		recorded := item.Bundle != "" && item.Bundle != interrupted
		switch {
		case item.Dir == keep || idle < inUse:
		case maxAge > 0 && idle > maxAge:
			out = append(out, item)
			continue
		case maxAge > 0 && recorded && !exists(item.Bundle):
			out = append(out, item)
			continue
		}
		left = append(left, item)
		total += item.Bytes
	}
	if maxBytes <= 0 || total <= maxBytes {
		return out
	}
	sort.SliceStable(left, func(i, j int) bool { return left[i].LastUsed.Before(left[j].LastUsed) })
	for _, item := range left {
		if total <= maxBytes {
			break
		}
		// An interrupted read may be an ingest still running elsewhere; only age removes it.
		if item.Dir == keep || now.Sub(item.LastUsed) < inUse || item.Bundle == interrupted {
			continue
		}
		out = append(out, item)
		total -= item.Bytes
	}
	return out
}

func envInt(name string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n >= 0 {
		return n
	}
	return fallback
}

// Remove deletes one store.
func Remove(dir string) error {
	// Only ever inside the cache: this deletes a directory tree, and the one thing it must not do
	// is delete one somebody's path happened to name.
	if filepath.Dir(dir) != CacheRoot() {
		return os.ErrInvalid
	}
	return os.RemoveAll(dir)
}
