package cli

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	reference "github.com/mongodb/ftdc"
	"go.mongodb.org/mongo-driver/bson"

	"mongo-log-parser/internal/ingest"
)

// A bundle collected twice holds the older metrics files in both copies. Ingest reads each file
// once; the commands that read samples afterwards must read the same files, not every file that
// happens to sit in a stream's directory.
func TestQueriesReadOnlyTheFTDCFilesIngestKept(t *testing.T) {
	start := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)
	first, second, third := chunkFile(t, start, 0), chunkFile(t, start.Add(time.Minute), 60), chunkFile(t, start.Add(2*time.Minute), 120)
	layout := map[string][]byte{
		"early/diagnostic.data/metrics.2026-03-04T05-00-00Z-00000": first,
		"early/diagnostic.data/metrics.2026-03-04T05-01-00Z-00000": second,
		"late/diagnostic.data/metrics.2026-03-04T05-01-00Z-00000":  second, // the copy
		"late/diagnostic.data/metrics.2026-03-04T05-02-00Z-00000":  third,
	}
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "on disk", true: "archived"}[archived], func(t *testing.T) {
			root := t.TempDir()
			if archived {
				writeTar(t, filepath.Join(root, "bundle.tar"), layout)
			} else {
				for name, data := range layout {
					path := filepath.Join(root, name)
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			storeDir := t.TempDir()
			st, err := ingest.Run(root, storeDir, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			set, err := openStreams(st, nil)
			if err != nil {
				t.Fatal(err)
			}
			if set.chunks != 3 {
				t.Fatalf("queries read %d chunks, want the 3 distinct files once each", set.chunks)
			}
			samples := 0
			for _, stream := range set.streams {
				read, err := stream.Read(time.Time{}, time.Time{}, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				samples += len(read.Timestamps)
			}
			if samples != 3*60 {
				t.Fatalf("queries read %d samples, want %d", samples, 3*60)
			}
			// Nothing half-written or duplicated is left where extracted streams are kept.
			_ = filepath.Walk(filepath.Join(storeDir, "ftdc"), func(path string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() && strings.HasPrefix(info.Name(), ".partial-") {
					t.Errorf("temporary file left behind: %s", path)
				}
				return nil
			})
		})
	}
}

func chunkFile(t *testing.T, start time.Time, offset int) []byte {
	t.Helper()
	collector := reference.NewBatchCollector(60)
	for i := range 60 {
		at := start.Add(time.Duration(i) * time.Second)
		if err := collector.Add(bson.D{{Key: "start", Value: at}, {Key: "serverStatus", Value: bson.D{
			{Key: "start", Value: at}, {Key: "opcounters", Value: bson.D{{Key: "query", Value: int64(offset + i)}}}}}}); err != nil {
			t.Fatal(err)
		}
	}
	payload, err := collector.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func writeTar(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := tar.NewWriter(f)
	for name, data := range files {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
