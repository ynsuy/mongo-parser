package store

import (
	"os"
	"path/filepath"
	"sort"
	"time"
)

// A store is kept until somebody removes it, because the whole point of one is to still be there
// when the next question is asked — a week later, when the ticket comes back. That makes the cache
// grow, so it is something a person has to be able to see and clear.

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
			out = append(out, Cached{Dir: dir, Bundle: "(interrupted read)", Bytes: dirBytes(dir)})
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

// Remove deletes one store.
func Remove(dir string) error {
	// Only ever inside the cache: this deletes a directory tree, and the one thing it must not do
	// is delete one somebody's path happened to name.
	if filepath.Dir(dir) != CacheRoot() {
		return os.ErrInvalid
	}
	return os.RemoveAll(dir)
}
