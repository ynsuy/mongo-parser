// Package store keeps one bundle's parsed contents in a local SQLite file, so a bundle is read once
// and every later question is a query rather than another pass over gigabytes of logs.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// SchemaVersion changes whenever a table changes shape or what ingest writes into it changes
// meaning; a store written under another version is rebuilt rather than misread.
const SchemaVersion = "5"

const schema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);

-- One physical input: a file on disk, or an entry inside an archive (container names the chain).
CREATE TABLE source (
  id INTEGER PRIMARY KEY,
  path TEXT NOT NULL,
  container TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  size INTEGER NOT NULL DEFAULT 0,
  note TEXT NOT NULL DEFAULT ''
);

-- What one process wrote inside one source. A mongod log file is one stream; a Kubernetes CSV
-- export carries every pod's lines interleaved, so it is one stream per pod.
CREATE TABLE stream (
  id INTEGER PRIMARY KEY,
  source INTEGER NOT NULL REFERENCES source(id),
  instance TEXT NOT NULL DEFAULT '',
  node INTEGER REFERENCES node(id),
  first_ts INTEGER, last_ts INTEGER,
  lines INTEGER NOT NULL DEFAULT 0,
  bad_lines INTEGER NOT NULL DEFAULT 0,
  bad_example TEXT NOT NULL DEFAULT '',
  identity TEXT NOT NULL DEFAULT '{}',
  first_block INTEGER, first_slot INTEGER, -- where its first and last lines are stored
  last_block INTEGER, last_slot INTEGER,
  truncated_lines INTEGER NOT NULL DEFAULT 0, -- lines the server itself cut short
  unparsed_attrs INTEGER NOT NULL DEFAULT 0   -- exported lines whose attributes stayed text
);

-- A mongod or mongos process. aliases holds every name it was seen under: the replica set config
-- name, the machine's host:port, a pod name, a file name.
CREATE TABLE node (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '', -- the shortest name that tells it apart from the others here
  aliases TEXT NOT NULL DEFAULT '[]',
  role TEXT NOT NULL DEFAULT '',
  version TEXT NOT NULL DEFAULT '',
  replset TEXT NOT NULL DEFAULT '',
  evidence TEXT NOT NULL DEFAULT ''
);

-- Log lines, a block of consecutive lines of one stream at a time (see blocks.go). ids is the
-- set of log ids in the block as ",id,id,", so a query for one can skip blocks without it.
CREATE TABLE block (
  id INTEGER PRIMARY KEY,
  stream INTEGER NOT NULL,
  first_ts INTEGER NOT NULL,
  last_ts INTEGER NOT NULL,
  lines INTEGER NOT NULL,
  sev_mask INTEGER NOT NULL,
  ids TEXT NOT NULL,
  data BLOB NOT NULL
);

-- One FTDC stream: a directory of metrics files written by one process.
CREATE TABLE ftdc (
  id INTEGER PRIMARY KEY,
  dir TEXT NOT NULL,
  origin TEXT NOT NULL,
  host TEXT NOT NULL DEFAULT '',
  node INTEGER REFERENCES node(id),
  first_ts INTEGER, last_ts INTEGER,
  chunks INTEGER NOT NULL DEFAULT 0,
  files INTEGER NOT NULL DEFAULT 0,
  paths TEXT NOT NULL DEFAULT '[]', -- the files it was read from, as a JSON array; never the whole dir
  note TEXT NOT NULL DEFAULT ''
);

-- Stretches of at least SilenceMinutes in which a node wrote no log line. others counts the lines
-- the rest of the bundle wrote meanwhile: a node silent while its peers kept logging was down,
-- hung, or its log for that stretch is missing; a node silent along with everyone is just quiet.
CREATE TABLE silence (
  node INTEGER NOT NULL REFERENCES node(id),
  from_ts INTEGER NOT NULL,
  to_ts INTEGER NOT NULL,
  others INTEGER NOT NULL,   -- about how many lines the other nodes wrote meanwhile
  before_block INTEGER NOT NULL, before_slot INTEGER NOT NULL, -- the node's last line before it
  after_block INTEGER NOT NULL, after_slot INTEGER NOT NULL    -- and its first line after
);

-- Anything else worth keeping whole: shell outputs, OS logs, collector summaries.
CREATE TABLE artifact (
  id INTEGER PRIMARY KEY,
  source INTEGER NOT NULL REFERENCES source(id),
  kind TEXT NOT NULL,
  content TEXT NOT NULL
);
`

// Indexes are built after the load: maintaining them row by row during a bulk insert costs several
// times what building them once does.
const indexes = `
CREATE INDEX block_time ON block(first_ts, last_ts);
CREATE INDEX block_stream ON block(stream, first_ts);
`

type Store struct {
	DB    *sql.DB
	Path  string
	cache blockCache
}

// Dir is where a bundle's store lives: under the user cache, keyed by the bundle's absolute path.
func Dir(root string) string {
	base := os.Getenv("MONGO_LOG_PARSER_CACHE")
	if base == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			cache = os.TempDir()
		}
		base = filepath.Join(cache, "mongo-log-parser")
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(base, filepath.Base(root)+"-"+hex.EncodeToString(sum[:])[:12])
}

func open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Create starts an empty store at dir, replacing whatever was there.
func Create(dir string) (*Store, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "store.db")
	db, err := open(path)
	if err != nil {
		return nil, err
	}
	for _, pragma := range []string{"PRAGMA journal_mode=OFF", "PRAGMA synchronous=OFF", "PRAGMA temp_store=MEMORY", "PRAGMA cache_size=-262144"} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, err
		}
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{DB: db, Path: path}, nil
}

// SilenceMinutes is the shortest stretch without log lines worth recording.
const SilenceMinutes = 15

// BuildIndexes runs once the bulk load is done.
func (s *Store) BuildIndexes() error {
	if _, err := s.DB.Exec(indexes); err != nil {
		return fmt.Errorf("indexes: %w", err)
	}
	return nil
}

// Finish records what the store was built from. A store without a fingerprint is an interrupted
// ingest and is never reused.
func (s *Store) Finish(meta map[string]string) error {
	meta["schema_version"] = SchemaVersion
	for key, value := range meta {
		if _, err := s.DB.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, key, value); err != nil {
			return err
		}
	}
	_, err := s.DB.Exec("ANALYZE")
	return err
}

// Open returns the store at dir if it was completed under this schema for this fingerprint.
func Open(dir, fingerprint string) (*Store, bool, error) {
	path := filepath.Join(dir, "store.db")
	if _, err := os.Stat(path); err != nil {
		return nil, false, nil
	}
	// A reader takes its shared lock once and keeps it. Left to SQLite's default, every block a
	// scan opened took and dropped the file lock again, and that was a third of reading the log.
	db, err := open(path + "?mode=ro&_pragma=locking_mode(EXCLUSIVE)")
	if err != nil {
		return nil, false, err
	}
	var version, print string
	_ = db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&version)
	_ = db.QueryRow(`SELECT value FROM meta WHERE key='fingerprint'`).Scan(&print)
	if version != SchemaVersion || print != fingerprint {
		db.Close()
		return nil, false, nil
	}
	markUsed(dir)
	return &Store{DB: db, Path: path}, true, nil
}

func (s *Store) Meta(key string) string {
	var value string
	_ = s.DB.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&value)
	return value
}

func (s *Store) Close() error { return s.DB.Close() }
