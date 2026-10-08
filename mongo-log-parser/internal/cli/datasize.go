package cli

import (
	"sort"
	"strings"
	"time"

	"mongo-log-parser/internal/collector"
	"mongo-log-parser/internal/ftdc"
	"mongo-log-parser/internal/store"
)

// How much data is there is a question with four answers in a bundle, and they are not the same
// number. This is the order they are asked in, most trustworthy first.
//
//   - An initial sync's statistics: the server's own figure for the logical size of the data, and
//     the only source that breaks it down by collection. Read from the logs; see initialsync.go.
//     Present only where a member was rebuilding while the logs were collected.
//   - The oplog's own stats from FTDC: exact, and there on every 7.0 and later bundle — but it is
//     the oplog and not the data.
//   - The data mount's used bytes from FTDC: exact, and there on 8.0. It is the whole filesystem,
//     so it counts the journal and anything else living on it, and it is what WiredTiger wrote
//     after compression rather than what the documents weigh.
//   - The same figure from a collector's pt-summary, which is how a 7.0 or earlier bundle has it.
//
// Every one of them is reported as what it is. A logical size and an on-disk size differ by the
// compression ratio — three or four times, with snappy — so presenting either as "the data size"
// without saying which it is turns a capacity question into a wrong answer.

// DataSize is one source's account of how much data there is.
type DataSize struct {
	// Source is the node the figure came from, and Kind what kind of figure it is.
	Source string `json:"source"`
	Kind   string `json:"kind"`

	// Oplog, where this is an oplog figure.
	OplogBytes     float64 `json:"oplog_bytes,omitempty"`
	OplogOnDisk    float64 `json:"oplog_disk_bytes,omitempty"`
	OplogMaxBytes  float64 `json:"oplog_max_bytes,omitempty"`
	OplogDocuments float64 `json:"oplog_documents,omitempty"`

	// Mount, where this is a filesystem figure.
	Mount         string  `json:"mount,omitempty"`
	MountCapacity float64 `json:"mount_capacity_bytes,omitempty"`
	MountUsed     float64 `json:"mount_used_bytes,omitempty"`
	// Rounded is set where the figure came from a report that had already rounded it — pt-summary
	// writes "1.5T" and "50%", so the bytes below are that arithmetic and not a measurement.
	Rounded bool `json:"rounded,omitempty"`
}

// The kinds of figure, which is what decides how each may be read.
const (
	// SizeOplog is the oplog's own stats: exact, and not the data.
	SizeOplog = "oplog"
	// SizeMount is a filesystem's used bytes: exact, on-disk after compression, and the whole
	// mount rather than MongoDB's share of it.
	SizeMount = "mount"
)

// collectorSizes is what a Percona collector's pt-summary says about the filesystems.
//
// It is the last link in the chain and the one that reaches furthest back: diagnostic data carries
// the mounts only from 8.0, and a collector has been writing this table for far longer. The figures
// are rounded to one decimal and a whole percent before they are written, so they are marked.
//
// The reports are read out of the store rather than off the disk: the bundle was walked once, at
// ingest, and a pt-summary inside an archive never had a path to walk to in the first place.
func collectorSizes(st *store.Store) []DataSize {
	found := make([]DataSize, 0, 4)
	for _, summary := range collector.Summaries(artifacts(st, "collector")) {
		name := summary.Host
		if name == "" {
			name = summary.Source
		}
		for _, filesystem := range summary.Filesystems {
			found = append(found, DataSize{
				Source: name, Kind: SizeMount, Mount: filesystem.Mount,
				MountCapacity: filesystem.Capacity, MountUsed: filesystem.UsedBytes, Rounded: true,
			})
		}
	}
	return found
}

// artifacts is every whole file the store kept under one kind, keyed by where it came from. The
// kind is matched on its head, so "collector" answers for "collector/pt-summary" too.
func artifacts(st *store.Store, kind string) map[string]string {
	rows, err := st.DB.Query(`
		SELECT source.path, artifact.content FROM artifact JOIN source ON source.id = artifact.source
		WHERE artifact.kind = ? OR artifact.kind LIKE ? || '/%'`, kind, kind)
	if err != nil {
		return nil
	}
	defer rows.Close()
	held := map[string]string{}
	for rows.Next() {
		var path, content string
		if err := rows.Scan(&path, &content); err != nil {
			return held
		}
		held[path] = content
	}
	return held
}

// oplogMetrics are the oplog's collection stats as FTDC carries them. MongoDB writes the oplog's
// stats into diagnostic data on every node from 7.0, which is why this works where nothing else does
// — and it writes no other collection's, which is why it cannot answer more than this.
var oplogMetrics = map[string]string{
	"size":        "local.oplog.rs.stats/storageStats/size",
	"storageSize": "local.oplog.rs.stats/storageStats/storageSize",
	"maxSize":     "local.oplog.rs.stats/storageStats/maxSize",
	"count":       "local.oplog.rs.stats/storageStats/count",
}

// mountPrefix is where 8.0 writes the filesystems it can see.
const mountPrefix = "systemMetrics/mounts/"

// dataSizes is everything the bundle can say about how much data there is, best source first.
//
// The diagnostic data is read one pass per stream, decoding only the handful of columns below — a
// stream is hundreds of megabytes and every one of these questions is answered by the last sample
// of four of them. A collector's pt-summary is asked only where the diagnostic data could not
// answer, because its figures were rounded before they were written.
//
// No window: "how much data is there" is a question about now, and the answer is the last reading
// of a gauge rather than anything that happened inside a period.
func dataSizes(st *store.Store) []DataSize {
	set, err := openStreams(st, nil)
	if err != nil {
		set = &streamSet{}
	}
	var from, to time.Time
	found := make([]DataSize, 0, 4)
	for i, stream := range set.streams {
		name := stream.Host()
		if name == "" && i < len(set.sources) {
			name = set.sources[i]
		}
		samples, err := stream.Read(from, to, sizeSelector, nil)
		if err != nil || samples == nil {
			continue
		}
		if oplog, ok := oplogSize(name, samples); ok {
			found = append(found, oplog)
		}
		found = append(found, mountSizes(name, samples)...)
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].Source != found[j].Source {
			return found[i].Source < found[j].Source
		}
		if found[i].Kind != found[j].Kind {
			return found[i].Kind < found[j].Kind
		}
		// Biggest mount first: the one holding the data is the one worth reading.
		return found[i].MountUsed > found[j].MountUsed
	})
	// And last, a collector's own report of the filesystems, which is how a bundle from before 8.0
	// has them at all. Only where the diagnostic data did not already say it, first-hand.
	if !hasMount(found) {
		found = append(found, collectorSizes(st)...)
	}
	return found
}

// sizeSelector keeps only the columns these figures come from.
func sizeSelector(path string) bool {
	if strings.HasPrefix(path, mountPrefix) {
		return strings.HasSuffix(path, "/capacity") || strings.HasSuffix(path, "/free")
	}
	for _, metric := range oplogMetrics {
		if path == metric {
			return true
		}
	}
	return false
}

// last is a metric's final reading, which is what "how much is there" asks for: these are gauges,
// and the newest one is the answer.
func last(samples *ftdc.Samples, path string) (float64, bool) {
	values := samples.Values[path]
	for i := len(values) - 1; i >= 0; i-- {
		if !isNaN(values[i]) {
			return values[i], true
		}
	}
	return 0, false
}

// isNaN without pulling in math for one comparison; a NaN is the only value not equal to itself.
func isNaN(value float64) bool { return value != value }

func oplogSize(source string, samples *ftdc.Samples) (DataSize, bool) {
	size, ok := last(samples, oplogMetrics["size"])
	if !ok {
		return DataSize{}, false
	}
	found := DataSize{Source: source, Kind: SizeOplog, OplogBytes: size}
	found.OplogOnDisk, _ = last(samples, oplogMetrics["storageSize"])
	found.OplogMaxBytes, _ = last(samples, oplogMetrics["maxSize"])
	found.OplogDocuments, _ = last(samples, oplogMetrics["count"])
	return found, true
}

// mountSizes is every filesystem the node could see, which on a database host is one mount holding
// the data and a dozen holding nothing of interest.
func mountSizes(source string, samples *ftdc.Samples) []DataSize {
	paths := map[string]bool{}
	for path := range samples.Values {
		// The mount is everything up to the last slash: a mount is named for its path and carries
		// slashes of its own, so only the final segment is the metric. Cutting the reversed string
		// at its first slash is what finds the last one.
		if rest, ok := strings.CutPrefix(path, mountPrefix); ok {
			if _, mount, cut := strings.Cut(reverse(rest), "/"); cut {
				paths[reverse(mount)] = true
			}
		}
	}
	found := make([]DataSize, 0, len(paths))
	for mount := range paths {
		capacity, haveCapacity := last(samples, mountPrefix+mount+"/capacity")
		free, haveFree := last(samples, mountPrefix+mount+"/free")
		if !haveCapacity || !haveFree || capacity <= 0 {
			continue
		}
		found = append(found, DataSize{
			Source: source, Kind: SizeMount, Mount: mount,
			MountCapacity: capacity, MountUsed: capacity - free,
		})
	}
	return found
}

// reverse is how the mount's own path is separated from the metric under it: a mount is named for
// its path and so carries slashes of its own, and only the last segment is the metric.
func reverse(text string) string {
	runes := []rune(text)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

// dataMount is the filesystem a deployment's data is most likely on: the biggest one the node has,
// excluding the roots that every machine has and no database uses.
//
// Naming it is a guess and it is labelled as one. The alternative — asking the reader to pick out
// the data mount from a list of a dozen — is a guess too, made by somebody with less to go on.
func dataMount(sizes []DataSize) (DataSize, bool) {
	best, found := DataSize{}, false
	for _, size := range sizes {
		if size.Kind != SizeMount || ignoredMount(size.Mount) {
			continue
		}
		if !found || size.MountCapacity > best.MountCapacity {
			best, found = size, true
		}
	}
	return best, found
}

// ignoredMount is a filesystem no deployment keeps its data on. Everything else is a candidate,
// including the root: a small deployment on a single disk is a real deployment.
func ignoredMount(mount string) bool {
	for _, prefix := range []string{"/dev", "/sys", "/proc", "/run", "/boot", "/tmp", "/var/tmp"} {
		if mount == prefix || strings.HasPrefix(mount, prefix+"/") {
			return true
		}
	}
	return false
}
