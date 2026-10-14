package analyze

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mongo-log-parser/internal/store"
)

// Process is what one server's log says the server itself is: which binary, which version, what it
// was configured with, and how often it came up.
//
// A triage that starts without this reads every other number without knowing what produced it — a
// queue depth means one thing on a 4 GB cache and another on 200 GB, and "the cluster is slow"
// after an upgrade is a different investigation from the same words on a steady version.
type Process struct {
	Source string `json:"source"`
	// Files are the log files this process was read from. A node's log is rotated, and a bundle
	// collects every rotation of it: several files per machine, which the report would count as
	// several mongods if Group did not put them back together.
	Files         []string `json:"files,omitempty"`
	Role          string   `json:"role"`
	Version       string   `json:"version,omitempty"`
	GitVersion    string   `json:"git_version,omitempty"`
	Modules       string   `json:"modules,omitempty"`
	Allocator     string   `json:"allocator,omitempty"`
	OS            string   `json:"os,omitempty"`
	Host          string   `json:"host,omitempty"`
	Port          int      `json:"port,omitempty"`
	DBPath        string   `json:"db_path,omitempty"`
	StorageEngine string   `json:"storage_engine,omitempty"`
	CacheGB       float64  `json:"cache_size_gb,omitempty"`
	ReplicaSet    string   `json:"replica_set,omitempty"`
	Authorization string   `json:"authorization,omitempty"`

	Starts   int `json:"starts"`
	Exits    int `json:"clean_exits"`
	Unclean  int `json:"unclean_shutdowns"`
	FCVMoves int `json:"fcv_changes"`

	// Versions is every version this log saw the process report. More than one means the binary
	// changed inside the window, which explains a break in every other number.
	Versions  []string  `json:"versions_seen,omitempty"`
	StartedAt []Startup `json:"started_at,omitempty"`
	FCV       string    `json:"feature_compatibility_version,omitempty"`

	// VersionAt is when the process reported Version, and LastStartAt when it last came up. Both
	// are needed because a bundle's log files are merged in no particular order: without a time
	// beside it, "the version" was whichever rotation the merge happened to reach first, which is
	// map iteration order and therefore a different answer on every run.
	VersionAt   time.Time `json:"version_reported_at,omitempty"`
	LastStartAt time.Time `json:"last_start_at,omitempty"`

	FirstAt time.Time `json:"first_at,omitzero"`
	LastAt  time.Time `json:"last_at,omitzero"`

	// What the FTDC says about the machine under the latest run.
	MemoryGB float64 `json:"memory_gb,omitempty"`
	CPUs     int64   `json:"cpus,omitempty"`
	// Changes is what differed from one run to the next: the binary, the cache it was given, the
	// machine under it. Each takes effect at a restart, and every counter breaks there.
	Changes []RunChange `json:"changes,omitempty"`
	// Endings is how runs ended, where the operating system said: killed by the OOM killer, aborted.
	Endings []Ending `json:"endings,omitempty"`

	versions map[string]bool
}

// RunChange is one thing that was different after a restart.
type RunChange struct {
	At   time.Time `json:"at"`
	What string    `json:"what"`
	From string    `json:"from"`
	To   string    `json:"to"`
}

// Ending is a run that ended other than by being asked to.
type Ending struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // oom_kill; signal (killed or dumped core); stop_kill (a stop that ended in SIGKILL); error (exited with a failure status)
	How  string    `json:"how"`
}

// Upgraded reports whether the process ran more than one version inside the window. Every counter
// in the bundle breaks at that point, and so does any comparison across it.
func (p Process) Upgraded() bool { return len(p.Versions) > 1 }

// Previous is the versions this process ran other than the current one.
func (p Process) Previous() []string {
	out := make([]string, 0, len(p.Versions))
	for _, version := range p.Versions {
		if version != p.Version {
			out = append(out, version)
		}
	}
	return out
}

// Startup is one time the process came up, and the version it came up on. The pair is what makes a
// version history readable: "5.0.0, then 6.0.0, then 5.0.0 again" is a rolled-back upgrade, and
// the same two version strings in a set say only that both were seen.
type Startup struct {
	At      time.Time `json:"at"`
	Version string    `json:"version,omitempty"`
	From    string    `json:"from,omitempty"` // where it was read when not from the log: ftdc, systemd
}

// Roles a process can be. A shard or config server is a mongod with a cluster role, and it is worth
// its own word because it changes what its logs are about.
const (
	RoleMongod = "mongod"
	RoleMongos = "mongos"
	RoleShard  = "shard server"
	RoleConfig = "config server"
)

// maxStarts bounds the restart times one process keeps; a crash loop can log thousands and the
// count already says it happened.
const maxStarts = 50

// oneStartup is how close together two startup lines have to be to be the same startup. A process
// writes several of them in the same breath — the greeting, the build info, the options — and they
// are seconds apart at most.
const oneStartup = 10 * time.Second

// startup counts a process coming up, from whichever of its startup lines arrived.
//
// Neither line can be relied on alone. "MongoDB starting" reads like the right marker and a mongos
// never writes it, so every router in a bundle reported no starts at all; "Build Info" is written
// by both, and a truncated log may hold one without the other. Taking either and collapsing the
// ones that arrive together counts each start once whichever line survived.
func (p *Process) startup(at time.Time) {
	// Compared as a distance, not as "later than": mongod stamps the greeting and the build info
	// with the same millisecond, so requiring the second one to be strictly later skipped the
	// check and counted every start twice.
	if !p.LastStartAt.IsZero() && at.Sub(p.LastStartAt).Abs() < oneStartup {
		if at.After(p.LastStartAt) {
			p.LastStartAt = at
		}
		return
	}
	p.Starts++
	if at.After(p.LastStartAt) {
		p.LastStartAt = at
	}
	if len(p.StartedAt) < maxStarts {
		p.StartedAt = append(p.StartedAt, Startup{At: at})
	}
}

// ranVersion records the version the startup nearest this moment came up on. Build Info arrives in
// the same breath as the greeting, so it names the start that was just counted.
func (p *Process) ranVersion(at time.Time, version string) {
	for i := len(p.StartedAt) - 1; i >= 0; i-- {
		if at.Sub(p.StartedAt[i].At).Abs() < oneStartup {
			p.StartedAt[i].Version = version
			return
		}
	}
}

// Deployment folds the startup and shutdown lines of every log into a picture of what was running.
type Deployment struct {
	processes map[string]*Process
	system    []store.SystemEvent
	from      time.Time // where the window begins, when Join was told
}

func NewDeployment() *Deployment { return &Deployment{processes: map[string]*Process{}} }

type startupAttributes struct {
	Port      int    `json:"port"`
	DBPath    string `json:"dbPath"`
	Host      string `json:"host"`
	BuildInfo struct {
		Version     string          `json:"version"`
		GitVersion  string          `json:"gitVersion"`
		Modules     json.RawMessage `json:"modules"`
		Allocator   string          `json:"allocator"`
		Environment json.RawMessage `json:"environment"`
	} `json:"buildInfo"`
	OS struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"os"`
	StorageEngine string `json:"storageEngine"`
	Config        string `json:"config"`
	Options       struct {
		Net struct {
			Port int `json:"port"`
		} `json:"net"`
		Storage struct {
			DBPath     string `json:"dbPath"`
			Engine     string `json:"engine"`
			WiredTiger struct {
				EngineConfig struct {
					CacheSizeGB float64 `json:"cacheSizeGB"`
				} `json:"engineConfig"`
			} `json:"wiredTiger"`
		} `json:"storage"`
		Sharding struct {
			ClusterRole string `json:"clusterRole"`
		} `json:"sharding"`
		Replication struct {
			ReplSet     string `json:"replSet"`
			ReplSetName string `json:"replSetName"`
		} `json:"replication"`
		Security struct {
			Authorization string `json:"authorization"`
		} `json:"security"`
	} `json:"options"`
	NewVersion string `json:"newVersion"`
}

// replicaSetConfig is the config document the server restates at every rotation. It is decoded on
// its own because "config" is a string on the WiredTiger line and a document on this one, and one
// struct cannot hold a field that is two types — which, decoded together, fails the whole line.
type replicaSetConfig struct {
	Config struct {
		Set     string `json:"_id"`
		Members []struct {
			Host string `json:"host"`
		} `json:"members"`
	} `json:"config"`
}

// cacheSize reads WiredTiger's effective cache out of the config string it logs at open. The
// configured size is often absent — most deployments take the default, which is half of RAM — and
// the default is exactly the number a reader needs and cannot get anywhere else.
var cacheSize = regexp.MustCompile(`cache_size=(\d+)([KMG]?)`)

// Add folds one record into the picture of the process that wrote it.
func (d *Deployment) Add(record Record) {
	message := record.Msg
	// Everything read here is written once or twice per process lifetime, so the cheap test comes
	// first: the overwhelming majority of a log is ordinary traffic that this never has to decode.
	if !deploymentMessage(message) && record.Ctx != "mongosMain" {
		return
	}
	process := d.processes[record.Source]
	if process == nil {
		process = &Process{Source: record.Source, Role: RoleMongod, versions: map[string]bool{}, FirstAt: record.At}
		d.processes[record.Source] = process
	}
	if record.At.Before(process.FirstAt) {
		process.FirstAt = record.At
	}
	if record.At.After(process.LastAt) {
		process.LastAt = record.At
	}
	// mongos names its startup thread, which is the one signal that does not depend on the wording
	// of any message.
	if record.Ctx == "mongosMain" && process.Role == RoleMongod {
		process.Role = RoleMongos
	}

	var attributes startupAttributes
	record.DecodeAttributes(&attributes)

	// A log rotation makes the server restate everything it said at startup — the host and port,
	// the build, the operating system, the command line, the replica set config, the feature
	// compatibility version — from the thread that handled the signal. It is the same set of lines
	// and it is not a start, and counting it as one reported a node whose log rotates on a schedule
	// as a node that restarts on that schedule, and each rotated file as a mongod of its own with
	// restarts that never happened.
	//
	// Everything in the banner is still read. On a deployment that has been up for months it is the
	// only place any of it appears.
	rotation := record.Ctx == "SignalHandler"

	switch {
	case strings.HasPrefix(message, "MongoDB starting"), strings.HasPrefix(message, "MongoS starting"),
		strings.HasPrefix(message, "mongos starting"):
		if !rotation {
			process.startup(record.At)
		}
		if attributes.Host != "" {
			process.Host = attributes.Host
		}
		if attributes.Port != 0 {
			process.Port = attributes.Port
		}
		if attributes.DBPath != "" {
			process.DBPath = attributes.DBPath
		}
		if strings.Contains(strings.ToLower(message), "mongos") {
			process.Role = RoleMongos
		}
	// A start is counted here rather than from "MongoDB starting", which is the line that reads
	// like the right one and is not: a mongos does not write it at all, so every router in a bundle
	// reported zero starts and no last-start time. Build Info is written exactly once per process
	// start by both binaries, and it carries the version that start was running.
	case message == "Build Info":
		if !rotation {
			process.startup(record.At)
		}
		if version := attributes.BuildInfo.Version; version != "" {
			process.versions[version] = true
			process.ranVersion(record.At, version)
			// The latest report wins, not the first one read. Build Info is written once per start,
			// so the newest of them is the version the process is running now.
			if record.At.After(process.VersionAt) {
				process.Version, process.VersionAt = version, record.At
				process.GitVersion = attributes.BuildInfo.GitVersion
				process.Allocator = attributes.BuildInfo.Allocator
				process.Modules = joinRaw(attributes.BuildInfo.Modules)
			}
		}
	// Process Details is the rotation banner's greeting, and on a long-running deployment it is the
	// only line in a bundle that says which machine a log belongs to. Without it the rotations of
	// one node cannot be told from the logs of several.
	case message == "Process Details":
		if attributes.Host != "" {
			process.Host = attributes.Host
		}
		if attributes.Port != 0 {
			process.Port = attributes.Port
		}
	// The set's own config, restated at every rotation, which names the set a node belongs to
	// whether or not the bundle caught it starting.
	case message == "Node is a member of a replica set":
		var config replicaSetConfig
		if record.DecodeAttributes(&config) && config.Config.Set != "" {
			process.ReplicaSet = replicaSetName(config.Config.Set)
		}
	case message == "Operating System":
		process.OS = strings.TrimSpace(attributes.OS.Name + " " + attributes.OS.Version)
	case strings.HasPrefix(message, "Options set by command line"):
		options := attributes.Options
		if options.Net.Port != 0 {
			process.Port = options.Net.Port
		}
		if options.Storage.DBPath != "" {
			process.DBPath = options.Storage.DBPath
		}
		if options.Storage.Engine != "" {
			process.StorageEngine = options.Storage.Engine
		}
		if size := options.Storage.WiredTiger.EngineConfig.CacheSizeGB; size > 0 {
			process.CacheGB = size
		}
		if set := firstNonEmpty(options.Replication.ReplSet, options.Replication.ReplSetName); set != "" {
			process.ReplicaSet = replicaSetName(set)
		}
		if options.Security.Authorization != "" {
			process.Authorization = options.Security.Authorization
		}
		switch options.Sharding.ClusterRole {
		case "configsvr":
			process.Role = RoleConfig
		case "shardsvr":
			process.Role = RoleShard
		}
	case strings.HasPrefix(message, "Storage engine to use detected by data files"):
		if attributes.StorageEngine != "" {
			process.StorageEngine = attributes.StorageEngine
		}
	case strings.HasPrefix(message, "Opening WiredTiger"), strings.HasPrefix(message, "wiredtiger_open config"):
		if process.StorageEngine == "" {
			process.StorageEngine = "wiredTiger"
		}
		if size, ok := parseCacheSize(attributes.Config); ok {
			process.CacheGB = size
		}
	case strings.HasPrefix(message, "Detected unclean shutdown"):
		process.Unclean++
	case message == "Now exiting", strings.HasPrefix(message, "Shutting down with code"):
		process.Exits++
	case strings.Contains(message, "featureCompatibilityVersion"):
		process.FCVMoves++
		if attributes.NewVersion != "" {
			process.FCV = attributes.NewVersion
		}
	}
}

// deploymentMessage is the cheap gate: a record whose message cannot be one of the lines read above
// never has its attributes decoded.
func deploymentMessage(message string) bool {
	switch message {
	case "Build Info", "Operating System", "Now exiting", "Process Details",
		"Node is a member of a replica set":
		return true
	}
	for _, prefix := range []string{
		"MongoDB starting", "MongoS starting", "mongos starting", "Options set by command line",
		"Storage engine to use detected by data files", "Opening WiredTiger", "wiredtiger_open config",
		"Detected unclean shutdown", "Shutting down with code",
	} {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}
	return strings.Contains(message, "featureCompatibilityVersion")
}

func parseCacheSize(config string) (float64, bool) {
	match := cacheSize.FindStringSubmatch(config)
	if match == nil {
		return 0, false
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, false
	}
	switch match[2] {
	case "K":
		return value / (1 << 20), true
	case "G":
		return value, true
	default: // WiredTiger reports megabytes when it prints no unit at all
		return value / 1024, true
	}
}

// replicaSetName drops the seed list a --replSet argument may carry: "rs0/a:27017,b:27017".
func replicaSetName(value string) string {
	if slash := strings.IndexByte(value, '/'); slash > 0 {
		return value[:slash]
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// joinRaw renders a small JSON value as text, for fields whose shape differs between versions.
func joinRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.Join(list, ",")
	}
	return strings.Trim(string(raw), `"`)
}

// Learn fills in what the startup lines could not say. A bundle whose logs were rotated past the
// last restart has no "MongoDB starting" in it at all, and the replication lines still name the set
// the node belongs to; without this the whole section would be empty for exactly the long-running
// deployments it matters most for.
func (d *Deployment) Learn(cluster *Cluster) {
	for source, node := range cluster.nodes {
		if node.ReplicaSet == "" {
			continue
		}
		process := d.processes[source]
		if process == nil {
			process = &Process{Source: source, Role: RoleMongod, versions: map[string]bool{}}
			d.processes[source] = process
		}
		if process.ReplicaSet == "" {
			process.ReplicaSet = node.ReplicaSet
		}
	}
}

// Processes returns what was running, grouped so the same replica set reads together.
func (d *Deployment) Processes() []Process {
	out := make([]Process, 0, len(d.processes))
	for _, process := range d.processes {
		copied := *process
		for version := range process.versions {
			copied.Versions = append(copied.Versions, version)
		}
		sort.Strings(copied.Versions)
		sort.Slice(copied.StartedAt, func(i, j int) bool { return copied.StartedAt[i].At.Before(copied.StartedAt[j].At) })
		out = append(out, copied)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ReplicaSet != out[j].ReplicaSet {
			return out[i].ReplicaSet < out[j].ReplicaSet
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// VersionChanges is the version history with the repeats removed: one entry per change, in order.
// A process that restarted several times on the same binary has one entry, which is the fact; a
// row per restart is the restart list, printed elsewhere.
func (p Process) VersionChanges() []Startup {
	out := make([]Startup, 0, 4)
	for _, start := range p.StartedAt {
		if start.Version == "" {
			continue
		}
		if len(out) > 0 && out[len(out)-1].Version == start.Version {
			continue
		}
		out = append(out, start)
	}
	return out
}

// Downgraded reports whether this process went back to a version it had already left behind. It is
// read off the order alone: a rollback is a version reappearing after a different one, and no
// comparison of version numbers is needed to see it.
func (p Process) Downgraded() bool {
	changes := p.VersionChanges()
	for i, start := range changes {
		for _, earlier := range changes[:i] {
			if earlier.Version == start.Version {
				return true
			}
		}
	}
	return false
}

// Summary is the one-line shape of the deployment: what kind of thing it is and what it runs.
type Summary struct {
	Processes   int      `json:"processes"`
	Roles       []string `json:"roles"`
	ReplicaSets []string `json:"replica_sets,omitempty"`
	Versions    []string `json:"versions,omitempty"`
	Engines     []string `json:"storage_engines,omitempty"`
	Sharded     bool     `json:"sharded"`
	Restarts    int      `json:"restarts"`
	Unclean     int      `json:"unclean_shutdowns"`
	OOMKills    int      `json:"oom_kills,omitempty"`
	Crashes     int      `json:"killed_or_crashed,omitempty"` // by a signal: an abort, a segfault, a kill
	FailedExits int      `json:"failed_exits,omitempty"`      // exited with a failure status, often a start that did not come up
	StopKills   int      `json:"stops_killed,omitempty"`      // asked to stop, and killed when it did not in time`
	NoAuth      int      `json:"processes_without_authorization"`
}

// Summarize reduces the processes to what a heading can say in one line.
func (d *Deployment) Summarize() Summary {
	summary := Summary{}
	roles, sets, versions, engines := map[string]int{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, process := range d.processes {
		summary.Processes++
		roles[process.Role]++
		if process.ReplicaSet != "" {
			sets[process.ReplicaSet] = true
		}
		for version := range process.versions {
			versions[version] = true
		}
		if process.StorageEngine != "" {
			engines[process.StorageEngine] = true
		}
		if process.Role == RoleMongos || process.Role == RoleConfig || process.Role == RoleShard {
			summary.Sharded = true
		}
		// A start is a restart unless it is where the window begins: a log that opens with the
		// server's greeting caught it starting, not restarting. One that opens mid-run and later
		// shows a start caught a restart.
		restarts := process.Starts
		if restarts > 0 && (d.from.IsZero() || earliest(process.StartedAt).Sub(d.from) < 10*time.Minute) {
			restarts--
		}
		summary.Restarts += restarts
		summary.Unclean += process.Unclean
		if process.Authorization == "disabled" {
			summary.NoAuth++
		}
	}
	for role, count := range roles {
		summary.Roles = append(summary.Roles, strconv.Itoa(count)+" "+role)
	}
	sort.Strings(summary.Roles)
	for _, process := range d.processes {
		for _, ending := range process.Endings {
			switch ending.Kind {
			case "oom_kill":
				summary.OOMKills++
			case "signal":
				summary.Crashes++
			case "stop_kill":
				summary.StopKills++
			default:
				summary.FailedExits++
			}
		}
	}
	summary.ReplicaSets = sortedKeys(sets)
	summary.Versions = sortedKeys(versions)
	summary.Engines = sortedKeys(engines)
	return summary
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// sameStart is how far apart two accounts of one start can be: systemd says it started the unit a
// moment before the server logs its greeting, and FTDC's uptime is read a moment after.
const sameStart = 2 * time.Minute

// started records a start read somewhere other than the log, unless the log or another source
// already has it.
func (p *Process) started(at time.Time, version, from string) {
	for i, start := range p.StartedAt {
		if start.At.Sub(at).Abs() < sameStart {
			if p.StartedAt[i].Version == "" {
				p.StartedAt[i].Version = version
			}
			return
		}
	}
	p.Starts++
	if at.After(p.LastStartAt) {
		p.LastStartAt = at
	}
	if len(p.StartedAt) < maxStarts {
		p.StartedAt = append(p.StartedAt, Startup{At: at, Version: version, From: from})
	}
}

func (d *Deployment) process(source string) *Process {
	process := d.processes[source]
	if process == nil {
		process = &Process{Source: source, Role: RoleMongod, versions: map[string]bool{}}
		d.processes[source] = process
	}
	return process
}

// Join adds what the FTDC and the operating system's logs say about each node's runs: starts the
// log did not catch, the version and cache of the latest run, the machine under it, what changed
// at each restart, and how runs that were not shut down ended. A node's log rarely covers its whole
// history; its FTDC usually covers the restarts the log lost, and only the OS saw it killed.
func (d *Deployment) Join(runs []store.Run, events []store.SystemEvent, from, to time.Time) {
	d.from = from
	inside := func(at time.Time) bool {
		return (from.IsZero() || !at.Before(from)) && (to.IsZero() || !at.After(to))
	}
	var previous *store.Run
	for i := range runs {
		run := &runs[i]
		if previous != nil && previous.Node != run.Node {
			previous = nil
		}
		if !inside(run.First) && !inside(run.Last) && !(run.First.Before(from) && run.Last.After(to)) {
			previous = run
			continue
		}
		p := d.process(run.Node)
		if p.ReplicaSet == "" {
			p.ReplicaSet = run.Set
		}
		switch {
		case strings.Contains(run.Role, "mongos"):
			p.Role = RoleMongos
		case strings.Contains(run.Role, "(shard)"):
			p.Role = RoleShard
		case strings.Contains(run.Role, "(config server)"):
			p.Role = RoleConfig
		}
		switch {
		case inside(run.Started):
			p.started(run.Started, run.Version, "ftdc")
		case run.Started.After(p.LastStartAt):
			// Up since before anything in the window: when it came up is still worth knowing.
			p.LastStartAt = run.Started
		}
		if run.Version != "" {
			p.versions[run.Version] = true
			if !run.Started.Before(p.VersionAt) {
				p.Version, p.VersionAt = run.Version, run.Started
			}
		}
		if run.CacheBytes > 0 {
			p.CacheGB = roundTenth(float64(run.CacheBytes) / (1 << 30))
		}
		if run.MemKB > 0 {
			p.MemoryGB = roundTenth(float64(run.MemKB) / (1 << 20))
		}
		if run.CPUs > 0 {
			p.CPUs = run.CPUs
		}
		if previous != nil {
			p.Changes = append(p.Changes, runChanges(*previous, *run)...)
		}
		previous = run
	}
	// systemd logs a stop it had to finish with SIGKILL as the process killed, then the unit
	// stopped, in the same second. A crash has no stop after it.
	stoppedAfter := func(i int) bool {
		for _, next := range events[i+1:] {
			if next.At.Sub(events[i].At) > 5*time.Second {
				return false
			}
			if next.Kind == "service_stop" && next.Host == events[i].Host {
				return true
			}
		}
		return false
	}
	for i, e := range events {
		if !inside(e.At) {
			continue
		}
		// A unit's starts and stops are read into the runs; listed again they are only noise.
		if e.Kind != "service_start" && e.Kind != "service_stop" {
			d.system = append(d.system, e)
		}
		if e.Node == "" || !strings.HasPrefix(e.Process, "mongo") {
			continue
		}
		p := d.process(e.Node)
		switch e.Kind {
		case "service_start":
			p.started(e.At, "", "systemd")
		case "oom_kill":
			p.ended(e.At, "oom_kill", "killed by the kernel's OOM killer ("+e.Detail+")")
		case "segfault":
			p.ended(e.At, "signal", "segfault ("+e.Detail+")")
		case "service_exit":
			switch {
			case strings.Contains(e.Detail, "status=0/SUCCESS"):
			case strings.Contains(e.Detail, "code=exited"):
				p.ended(e.At, "error", e.Process+".service exited: "+e.Detail)
			case stoppedAfter(i):
				p.ended(e.At, "stop_kill", "stopped by systemd, and killed when it did not exit in time: "+e.Detail)
			default:
				p.ended(e.At, "signal", e.Process+".service exited: "+e.Detail)
			}
		}
	}
	for _, p := range d.processes {
		killed := 0
		for _, e := range p.Endings {
			if e.Kind != "error" {
				killed++
			}
		}
		p.Unclean = max(p.Unclean, killed)
	}
}

// ended records how a run ended. The kernel's kill and systemd's account of the process going are
// one ending.
func (p *Process) ended(at time.Time, kind, how string) {
	if n := len(p.Endings); n > 0 && kind != "oom_kill" && p.Endings[n-1].Kind == "oom_kill" && at.Sub(p.Endings[n-1].At).Abs() < sameStart {
		p.Endings[n-1].How += "; " + how
		return
	}
	p.Endings = append(p.Endings, Ending{At: at, Kind: kind, How: how})
}

func runChanges(before, after store.Run) []RunChange {
	var out []RunChange
	add := func(what, from, to string) {
		if from != "" && to != "" && from != to {
			out = append(out, RunChange{At: after.Started, What: what, From: from, To: to})
		}
	}
	gib := func(bytes int64, unit float64) string {
		if bytes <= 0 {
			return ""
		}
		return strconv.FormatFloat(roundTenth(float64(bytes)/unit), 'f', -1, 64) + "G"
	}
	count := func(n int64) string {
		if n <= 0 {
			return ""
		}
		return strconv.FormatInt(n, 10)
	}
	add("version", before.Version, after.Version)
	add("cache", gib(before.CacheBytes, 1<<30), gib(after.CacheBytes, 1<<30))
	add("memory", gib(before.MemKB, 1<<20), gib(after.MemKB, 1<<20))
	add("cpus", count(before.CPUs), count(after.CPUs))
	return out
}

func earliest(starts []Startup) time.Time {
	var first time.Time
	for _, s := range starts {
		if first.IsZero() || s.At.Before(first) {
			first = s.At
		}
	}
	return first
}

func roundTenth(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// SystemEvents is what the operating system's logs said inside the window, oldest first.
func (d *Deployment) SystemEvents() []store.SystemEvent {
	if d.system == nil {
		return []store.SystemEvent{}
	}
	return d.system
}
