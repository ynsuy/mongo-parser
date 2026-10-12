package ingest

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Tying streams to nodes. A node is a mongod or mongos process; it may have written many rotated
// log files, appear as one pod's rows in an export, and have an FTDC stream beside them. Which of
// those are one process is decided by what each says about itself, strongest evidence first:
//
//  1. the same name: the replica set config name ("Found self in config", FTDC's repl.me), the
//     machine's host:port ("MongoDB starting", FTDC's serverStatus.host), a pod name;
//  2. the same short hostname and port, because one side says ip-10-0-0-5.ec2.internal:27017 and
//     the other ip-10-0-0-5:27017;
//  3. a name without a port (a pod) joins the one node with that short hostname, never several;
//  4. a file that identified itself nowhere joins the one node identified in its directory;
//  5. a file that identified itself nowhere joins the one node whose host name its path spells out,
//     because a collector that writes db1_logs/mongodb.log beside that node's FTDC has
//     said whose log it is in the only place left;
//  6. otherwise it is a node of its own, named after its file, and marked as such.

type item struct {
	stream   *streamRow
	ftdc     *ftdcRow
	names    []string // full names, lower case
	evidence []string
}

func (it *item) dir() string {
	if it.stream != nil {
		return it.stream.Dir
	}
	return it.ftdc.Origin
}

// nodeDir is the directory that stands for a node: the item's own, or the first one above it
// whose name is not one every node shares. mongod writes FTDC into diagnostic.data whichever node
// it is, so node-a/diagnostic.data belongs to node-a.
func nodeDir(dir string) string {
	for {
		i := strings.LastIndexAny(dir, "/!")
		if i < 0 || !generic[strings.ToLower(dir[i+1:])] {
			return dir
		}
		dir = dir[:i]
	}
}

// path is where this came from, archives included: the directory a name may be written on, plus
// the file's own name, which sometimes carries it instead.
func (it *item) path() string {
	if it.stream != nil {
		return it.stream.Dir + "/" + it.stream.Name
	}
	return it.ftdc.Origin
}

// namesHost reports whether a lower-cased path spells out a host name as a name of its own, with
// a separator or an end on each side. mongo1 is not found inside mongo11, and rs0-2 is found in
// rs0-2_logs/mongodb.log, where the underscore ends it.
func namesHost(where, host string) bool {
	for at := 0; at+len(host) <= len(where); {
		i := strings.Index(where[at:], host)
		if i < 0 {
			return false
		}
		i += at
		if !nameByte(where, i-1) && !nameByte(where, i+len(host)) {
			return true
		}
		at = i + 1
	}
	return false
}

// nameByte reports whether the byte at i is one a host name could continue with. Out of range is
// an end, and so is any punctuation a path separates on.
func nameByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	b := s[i]
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

type unionFind []int

func (u unionFind) find(i int) int {
	for u[i] != i {
		u[i] = u[u[i]]
		i = u[i]
	}
	return i
}

func (u unionFind) join(a, b int) { u[u.find(a)] = u.find(b) }

func shortHost(name string) (string, string) {
	host, port := name, ""
	if i := strings.LastIndexByte(name, ':'); i > 0 {
		host, port = name[:i], name[i+1:]
	}
	if i := strings.IndexByte(host, '.'); i > 0 && !isIP(host) {
		host = host[:i]
	}
	return strings.ToLower(host), port
}

var ipv4 = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

// qualified reports whether a name says which machine it is beyond its short hostname: a domain
// or an IP address.
func qualified(name string) bool {
	host := name
	if i := strings.LastIndexByte(name, ':'); i > 0 {
		host = name[:i]
	}
	return strings.Contains(host, ".")
}

func isIP(host string) bool { return ipv4.MatchString(host) }

func lowerAll(names ...string) []string {
	var out []string
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			out = append(out, n)
		}
	}
	return out
}

type nodeRow struct {
	ID       int64
	Name     string
	Label    string
	fallback bool // named after a file, not a host
	Aliases  []string
	Role     string
	Versions []string
	ReplSet  string
	Evidence []string
	members  []*item
}

// ftdcHost is the name serverStatus.host stands for. mongod writes its port there only when it is
// not the default (prettyHostName): a node on 27017 reports "db1", one on 27018 "db1:27018". Read as
// written, "db1" is a name without a port, and joined nothing on a machine with no domain name —
// its own log, which says "db1:27017", stayed a separate node, and the labels drawn from its FTDC
// could not be read beside its log.
func ftdcHost(host string) string {
	if host == "" || strings.Contains(host, ":") {
		return host
	}
	return host + ":27017"
}

func resolveNodes(streams []streamRow, ftdcs []ftdcRow) []*nodeRow {
	var items []*item
	for i := range streams {
		s := &streams[i]
		id := s.Stats.Identity
		it := &item{stream: s}
		if id.Self != "" {
			it.names = append(it.names, lowerAll(id.Self)...)
			it.evidence = append(it.evidence, "log: its name in the replica set config")
		}
		if id.Host != "" && id.Port != 0 {
			it.names = append(it.names, lowerAll(fmt.Sprintf("%s:%d", id.Host, id.Port))...)
			it.evidence = append(it.evidence, "log: host and port at startup")
		}
		if id.Pod != "" {
			it.names = append(it.names, lowerAll(id.Pod)...)
			it.evidence = append(it.evidence, "log export: instance column")
		}
		items = append(items, it)
	}
	for i := range ftdcs {
		f := &ftdcs[i]
		it := &item{ftdc: f}
		it.names = lowerAll(ftdcHost(f.Identity.Host), f.Identity.Me)
		if len(it.names) > 0 {
			it.evidence = append(it.evidence, "ftdc: serverStatus")
		}
		items = append(items, it)
	}

	uf := make(unionFind, len(items))
	for i := range uf {
		uf[i] = i
	}
	// 1. Exact full names. A short name (no domain: a pod name, an unqualified hostname) is not
	// used here: in a replica set spread over datacenters, mongodb-rs0-0:27017 can be three
	// different machines that each call themselves that.
	byName := map[string]int{}
	for i, it := range items {
		for _, n := range it.names {
			if !qualified(n) {
				continue
			}
			if j, ok := byName[n]; ok {
				uf.join(i, j)
			} else {
				byName[n] = i
			}
		}
	}
	// 2. Short names link only when they are unambiguous: at most one group has a full name with
	// that short form. Then everything carrying the short form joins it. Otherwise nothing joins on
	// it, and the items keep the groups they have.
	claim := func(n string) string {
		host, port := shortHost(n)
		return host + ":" + port
	}
	type claimants struct {
		full map[int]bool
		weak []int
	}
	keys := map[string]*claimants{}
	get := func(k string) *claimants {
		if keys[k] == nil {
			keys[k] = &claimants{full: map[int]bool{}}
		}
		return keys[k]
	}
	for i, it := range items {
		for _, n := range it.names {
			c := get(claim(n))
			if qualified(n) {
				c.full[uf.find(i)] = true
			} else {
				c.weak = append(c.weak, i)
			}
			// A portless name also claims every port of its host.
			if _, port := shortHost(n); port == "" {
				continue
			}
			host, _ := shortHost(n)
			hc := get(host + ":")
			if qualified(n) {
				hc.full[uf.find(i)] = true
			}
		}
	}
	for k, c := range keys {
		if len(c.full) > 1 {
			for _, i := range c.weak {
				full := false
				for _, n := range items[i].names {
					full = full || qualified(n)
				}
				if !full {
					items[i].evidence = append(items[i].evidence, fmt.Sprintf(
						"ambiguous: %d nodes here go by the short name %s, and nothing in this one says which it is", len(c.full), strings.TrimSuffix(k, ":")))
				}
			}
			continue
		}
		anchor := -1
		for g := range c.full {
			anchor = g
		}
		for _, i := range c.weak {
			if anchor < 0 {
				anchor = i
				continue
			}
			uf.join(i, anchor)
		}
	}
	// 4. Unidentified items join the one identified group in their directory.
	identifiedIn := map[string]map[int]bool{}
	for i, it := range items {
		if len(it.names) == 0 {
			continue
		}
		d := it.dir()
		if identifiedIn[d] == nil {
			identifiedIn[d] = map[int]bool{}
		}
		identifiedIn[d][uf.find(i)] = true
	}
	placed := make([]bool, len(items))
	for i, it := range items {
		if len(it.names) > 0 {
			continue
		}
		groups := identifiedIn[it.dir()]
		if len(groups) == 1 {
			for g := range groups {
				uf.join(i, g)
			}
			placed[i] = true
			it.evidence = append(it.evidence, "no identity of its own: the only node identified in its directory")
		}
	}
	// 5. What is left joins the node its own path names. A bundle is very often one directory or
	// archive per host, with that host's name on it and nothing inside the rotated logs repeating
	// it, and then the path is the only evidence there is. It is weaker than a line of the log, so
	// it counts only where the name belongs to exactly one node and only one such name is in the
	// path — and it says in the node's evidence that this is where the name came from.
	hostRoots := map[string]map[int]bool{}
	for i, it := range items {
		for _, n := range it.names {
			host, _ := shortHost(n)
			if len(host) < 3 || generic[host] {
				continue
			}
			if hostRoots[host] == nil {
				hostRoots[host] = map[int]bool{}
			}
			hostRoots[host][uf.find(i)] = true
		}
	}
	for i, it := range items {
		if len(it.names) > 0 || placed[i] {
			continue
		}
		where := strings.ToLower(it.path())
		found, root := "", -1
		for host, roots := range hostRoots {
			if len(roots) != 1 || !namesHost(where, host) {
				continue
			}
			for g := range roots {
				if root >= 0 && g != root {
					root = -1 // two different nodes are named in this path; it settles nothing.
					break
				}
				found, root = host, g
			}
			if root < 0 {
				break
			}
		}
		if root >= 0 {
			uf.join(i, root)
			placed[i] = true
			it.evidence = append(it.evidence, "no identity of its own: its path names "+found)
		}
	}
	// 6. The rest group by directory and file stem.
	byStem := map[string]int{}
	for i, it := range items {
		if len(it.names) > 0 || placed[i] {
			continue
		}
		key := it.dir() + "|" + fallbackName(it)
		if j, ok := byStem[key]; ok {
			uf.join(i, j)
		} else {
			byStem[key] = i
		}
		it.evidence = append(it.evidence, "file name only: no line in it says which process wrote it")
	}
	// 7. An FTDC stream left on its own joins the logs left on their own in its node's directory,
	// when there is one of each there: a collector that wrote node-a/mongod.log beside
	// node-a/diagnostic.data has said they are one process in the only place left.
	alone := func(stream bool) map[string]map[int]bool {
		out := map[string]map[int]bool{}
		for i, it := range items {
			if len(it.names) > 0 || placed[i] || (it.stream != nil) != stream {
				continue
			}
			d := nodeDir(it.dir())
			if out[d] == nil {
				out[d] = map[int]bool{}
			}
			out[d][uf.find(i)] = true
		}
		return out
	}
	logsAlone, ftdcAlone := alone(true), alone(false)
	for d, ftdcGroups := range ftdcAlone {
		if len(ftdcGroups) != 1 || len(logsAlone[d]) != 1 {
			continue
		}
		for f := range ftdcGroups {
			for l := range logsAlone[d] {
				uf.join(f, l)
			}
		}
		for i, it := range items {
			if it.ftdc != nil && len(it.names) == 0 && !placed[i] && nodeDir(it.dir()) == d {
				it.evidence = append(it.evidence, "no identity of its own: the only FTDC beside the only unnamed log in "+d)
			}
		}
	}

	groups := map[int][]*item{}
	var order []int
	for i, it := range items {
		root := uf.find(i)
		if _, ok := groups[root]; !ok {
			order = append(order, root)
		}
		groups[root] = append(groups[root], it)
	}
	var nodes []*nodeRow
	for _, root := range order {
		nodes = append(nodes, describe(groups[root]))
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].ReplSet != nodes[j].ReplSet {
			return nodes[i].ReplSet < nodes[j].ReplSet
		}
		return nodes[i].Name < nodes[j].Name
	})
	for i, n := range nodes {
		n.ID = int64(i + 1)
	}
	labelNodes(nodes)
	return nodes
}

// labelNodes gives every node the shortest name that still tells it apart: its short hostname,
// with the port when two share a host, and with the one domain word only it has when two share a
// short hostname (mongodb-rs0-0@dc1 and mongodb-rs0-0@dc2).
func labelNodes(nodes []*nodeRow) {
	short := map[string][]*nodeRow{}
	for _, n := range nodes {
		if n.fallback {
			n.Label = n.Name
			continue
		}
		host, _ := shortHost(n.Name)
		short[host] = append(short[host], n)
	}
	for host, group := range short {
		ports := map[string]int{}
		for _, n := range group {
			_, port := shortHost(n.Name)
			ports[port]++
		}
		for _, n := range group {
			_, port := shortHost(n.Name)
			label := host
			if len(ports) > 1 && port != "" {
				label += ":" + port
			}
			if len(group) > 1 && ports[port] > 1 {
				if word := uniqueWord(n, group, nodes); word != "" {
					label += "@" + word
				} else {
					label = n.Name
				}
			}
			n.Label = label
		}
	}
}

func domainWords(name string) []string {
	if i := strings.LastIndexByte(name, ':'); i > 0 {
		name = name[:i]
	}
	parts := strings.Split(strings.ToLower(name), ".")
	if len(parts) < 2 {
		return nil
	}
	return parts[1:]
}

// uniqueWord picks the domain word that tells n apart from the others sharing its short name.
// A good one names a site: it is carried by at most one node per short hostname across the whole
// bundle (dc1 is on rs0-0, rs0-1 and rs0-2 once each), unlike a word every site's domain ends in.
// Among those, short tokens like dc1, east or az1 are preferred, then shorter, then later in the
// name, where datacenters and regions usually sit.
func uniqueWord(n *nodeRow, group []*nodeRow, all []*nodeRow) string {
	others := map[string]bool{}
	for _, o := range group {
		if o == n {
			continue
		}
		for _, w := range domainWords(o.Name) {
			others[w] = true
		}
	}
	perHost := map[string]map[string]int{}
	for _, o := range all {
		host, _ := shortHost(o.Name)
		for _, w := range domainWords(o.Name) {
			if perHost[w] == nil {
				perHost[w] = map[string]int{}
			}
			perHost[w][host]++
		}
	}
	siteLike := func(w string) bool {
		for _, count := range perHost[w] {
			if count > 1 {
				return false
			}
		}
		return true
	}
	best, bestScore := "", -1
	words := domainWords(n.Name)
	for position, w := range words {
		if others[w] {
			continue
		}
		// The last word is the top-level domain. It tells two sites apart only by accident — "net"
		// beside "local" — and names neither, so it is the choice of last resort.
		if position == len(words)-1 && position > 0 && best != "" {
			continue
		}
		score := position
		if siteLike(w) {
			score += 10000
		}
		if shortToken.MatchString(w) {
			score += 1000
		}
		score += (100 - min(len(w), 99)) * 10
		if score > bestScore {
			best, bestScore = w, score
		}
	}
	return best
}

var shortToken = regexp.MustCompile(`^[a-z]{1,4}\d*$`)

func describe(members []*item) *nodeRow {
	n := &nodeRow{members: members}
	pick := func(get func(*item) string) string {
		for _, it := range members {
			if v := get(it); v != "" {
				return v
			}
		}
		return ""
	}
	n.Name = pick(func(it *item) string {
		if it.stream != nil {
			return it.stream.Stats.Identity.Self
		}
		return it.ftdc.Identity.Me
	})
	if n.Name == "" {
		n.Name = pick(func(it *item) string {
			if it.stream != nil && it.stream.Stats.Identity.Host != "" && it.stream.Stats.Identity.Port != 0 {
				return fmt.Sprintf("%s:%d", it.stream.Stats.Identity.Host, it.stream.Stats.Identity.Port)
			}
			if it.ftdc != nil {
				return it.ftdc.Identity.Host
			}
			return ""
		})
	}
	if n.Name == "" {
		n.Name = pick(func(it *item) string {
			if it.stream != nil {
				return it.stream.Stats.Identity.Pod
			}
			return ""
		})
	}
	if n.Name == "" {
		// An FTDC stream is named after its node's directory, a log after its file as well, so
		// the stream is the plainer name for the two together.
		named := members[0]
		for _, it := range members {
			if it.ftdc != nil {
				named = it
				break
			}
		}
		n.Name = fallbackName(named)
		n.fallback = true
	}

	aliases := map[string]bool{}
	evidence := map[string]bool{}
	type versionAt struct {
		v  string
		at int64
	}
	var versions []versionAt
	process, role := "", ""
	for _, it := range members {
		for _, a := range it.names {
			aliases[a] = true
		}
		for _, e := range it.evidence {
			evidence[e] = true
		}
		if it.stream != nil {
			id := it.stream.Stats.Identity
			for _, v := range id.Versions {
				versions = append(versions, versionAt{v, it.stream.Stats.First})
			}
			if id.Process != "" {
				process = id.Process
			}
			if id.ClusterRole != "" {
				role = id.ClusterRole
			}
			if id.ReplSet != "" && n.ReplSet == "" {
				n.ReplSet = id.ReplSet
			}
		} else {
			id := it.ftdc.Identity
			if id.Version != "" {
				versions = append(versions, versionAt{id.Version, it.ftdc.First})
			}
			if id.Process != "" && process == "" {
				process = id.Process
			}
			if id.SetName != "" && n.ReplSet == "" {
				n.ReplSet = id.SetName
			}
		}
	}
	sort.SliceStable(versions, func(i, j int) bool { return versions[i].at < versions[j].at })
	for _, v := range versions {
		if len(n.Versions) == 0 || n.Versions[len(n.Versions)-1] != v.v {
			n.Versions = append(n.Versions, v.v)
		}
	}
	n.Role = strings.TrimSpace(process + " " + roleName(role))
	for a := range aliases {
		n.Aliases = append(n.Aliases, a)
	}
	sort.Strings(n.Aliases)
	for e := range evidence {
		n.Evidence = append(n.Evidence, e)
	}
	sort.Strings(n.Evidence)
	return n
}

func roleName(clusterRole string) string {
	switch clusterRole {
	case "shardsvr":
		return "(shard)"
	case "configsvr":
		return "(config server)"
	}
	return ""
}

var (
	casePrefix   = regexp.MustCompile(`^CS\d+_`)
	rotationDate = regexp.MustCompile(`[-_.]?\d{8}(T?[\d-]+Z?)?$|\.\d+$`)
	generic      = map[string]bool{"mongod": true, "mongodb": true, "mongos": true, "mongo": true, "metrics": true, "diagnostic.data": true}
)

// fallbackName names something from where it sat when nothing in it said what it was.
func fallbackName(it *item) string {
	var name, dir string
	if it.stream != nil {
		name, dir = it.stream.Name, it.stream.Dir
	} else {
		name, dir = "", it.ftdc.Origin
	}
	stem := casePrefix.ReplaceAllString(name, "")
	if i := strings.Index(stem, ".log"); i > 0 {
		stem = stem[:i]
	}
	stem = strings.TrimSuffix(stem, ".gz")
	stem = rotationDate.ReplaceAllString(stem, "")
	where := nodeDir(dir)
	if i := strings.LastIndex(where, "!"); i >= 0 {
		where = where[i+1:]
	}
	where = casePrefix.ReplaceAllString(path.Base(where), "")
	if where == "." || where == "/" {
		where = ""
	}
	if stem == "" || generic[strings.ToLower(stem)] {
		if where == "" {
			return "unnamed (" + name + ")"
		}
		if stem == "" {
			return where
		}
		return where + "/" + stem
	}
	return stem
}

func (in *ingester) writeCatalogue(ftdcs []ftdcRow) ([]*nodeRow, error) {
	nodes := resolveNodes(in.streams, ftdcs)
	tx, err := in.st.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, s := range in.sources {
		if _, err := tx.Exec(`INSERT INTO source(id, path, container, kind, size, note) VALUES (?,?,?,?,?,?)`,
			s.ID, s.Path, s.Container, kindWithSub(s), s.Size, s.Note); err != nil {
			return nil, err
		}
		if s.Artifact != nil {
			if _, err := tx.Exec(`INSERT INTO artifact(source, kind, content) VALUES (?,?,?)`, s.ID, kindWithSub(s), *s.Artifact); err != nil {
				return nil, err
			}
		}
	}
	nodeOf := map[*item]int64{}
	for _, n := range nodes {
		aliases, _ := json.Marshal(n.Aliases)
		if _, err := tx.Exec(`INSERT INTO node(id, name, label, aliases, role, version, replset, evidence) VALUES (?,?,?,?,?,?,?,?)`,
			n.ID, n.Name, n.Label, string(aliases), n.Role, strings.Join(n.Versions, " → "), n.ReplSet, strings.Join(n.Evidence, "; ")); err != nil {
			return nil, err
		}
		for _, it := range n.members {
			nodeOf[it] = n.ID
		}
	}
	for _, n := range nodes {
		for _, it := range n.members {
			if it.stream != nil {
				s := it.stream
				identity, _ := json.Marshal(s.Stats.Identity)
				var first, last, fb, fs, lb, ls any
				truncated := 0
				if s.Stats.Lines > 0 {
					first, last = s.Stats.First, s.Stats.Last
				}
				if b := s.built; b != nil && b.has {
					fb, fs, lb, ls = b.first.Block, b.first.Slot, b.last.Block, b.last.Slot
					truncated = b.truncated
				}
				if _, err := tx.Exec(`INSERT INTO stream(id, source, instance, node, first_ts, last_ts, lines, bad_lines, bad_example, identity,
					first_block, first_slot, last_block, last_slot, truncated_lines, unparsed_attrs) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					s.ID, s.Source, s.Instance, n.ID, first, last, s.Stats.Lines, s.Stats.BadLines, s.Stats.BadExample, string(identity),
					fb, fs, lb, ls, truncated, s.Stats.Unparsed); err != nil {
					return nil, err
				}
				continue
			}
			f := it.ftdc
			var first, last any
			if f.Chunks > 0 {
				first, last = f.First, f.Last
			}
			paths, err := json.Marshal(f.Paths)
			if err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`INSERT INTO ftdc(dir, origin, host, node, first_ts, last_ts, chunks, files, paths, note) VALUES (?,?,?,?,?,?,?,?,?,?)`,
				f.Dir, f.Origin, f.Identity.Host, n.ID, first, last, f.Chunks, f.Files, string(paths), f.Note); err != nil {
				return nil, err
			}
		}
	}
	return nodes, tx.Commit()
}

func kindWithSub(s sourceRow) string {
	if s.Sub == "" {
		return s.Kind
	}
	return s.Kind + "/" + s.Sub
}
