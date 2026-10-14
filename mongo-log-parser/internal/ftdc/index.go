package ftdc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// The index of a capture is expensive to build and cheap to store, and the agent runs one process
// per question, so it is kept on disk and reused until the file changes.
type cachedIndex struct {
	Version int         `json:"version"`
	Path    string      `json:"path"`
	Size    int64       `json:"size"`
	ModTime time.Time   `json:"mod_time"`
	Chunks  []ChunkInfo `json:"chunks"`
	Last    time.Time   `json:"last"`
}

// indexVersion changes whenever the way a file is indexed changes. The cache is keyed on the file,
// and a file that has not changed is still indexed differently by a decoder that has: version 1
// accepted any document with a plausible length, took a corrupt one at the end of a damaged capture
// for a chunk, and cached the file as whole. Every index written by it is refused and rebuilt, so the
// damage it missed is found and reported rather than remembered as clean. Version 2 did not know
// when a file's last sample was taken, and version 3 what each chunk says about its process.
const indexVersion = 4

func cacheDir() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "mongo-log-parser", "index")
}

func cachePath(path string) string {
	dir := cacheDir()
	if dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(dir, hex.EncodeToString(sum[:16])+".json")
}

// loadIndex returns the cached index when it still describes the file on disk.
func loadIndex(path string, info os.FileInfo) *cachedIndex {
	name := cachePath(path)
	if name == "" {
		return nil
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return nil
	}
	var cached cachedIndex
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil
	}
	if cached.Version != indexVersion || cached.Path != path || cached.Size != info.Size() || !cached.ModTime.Equal(info.ModTime()) {
		return nil
	}
	return &cached
}

// saveIndex stores an index. A cache write is never worth failing a command over.
func saveIndex(path string, info os.FileInfo, chunks []ChunkInfo, last time.Time) {
	name := cachePath(path)
	if name == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(cachedIndex{Version: indexVersion, Path: path, Size: info.Size(), ModTime: info.ModTime(), Chunks: chunks, Last: last})
	if err != nil {
		return
	}
	temporary := name + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(temporary, name)
}
