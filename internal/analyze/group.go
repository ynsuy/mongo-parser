package analyze

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Category is the coarse bucket a log line falls into; it is navigation, not a diagnosis.
type Category string

const (
	SlowQuery   Category = "slow_query"
	Election    Category = "election"
	InitialSync Category = "initial_sync"
	Recovery    Category = "recovery"
	Replication Category = "replication"
	Sharding    Category = "sharding"
	// Catalogue is the constant, not DDL, because DDL is the accumulator that reads these lines.
	Catalogue      Category = "ddl"
	Storage        Category = "storage"
	Network        Category = "network"
	Authentication Category = "authentication"
	Query          Category = "query"
	Other          Category = "other"
)

// Group is a set of log lines that say the same thing, counted over the window.
type Group struct {
	ID        string         `json:"group_id"`
	Category  Category       `json:"category"`
	Severity  string         `json:"severity"`
	Component string         `json:"component"`
	MessageID int            `json:"message_id"`
	Message   string         `json:"message"`
	Count     int            `json:"count"`
	First     time.Time      `json:"first_at,omitzero"`
	Last      time.Time      `json:"last_at,omitzero"`
	Sources   map[string]int `json:"sources"`
	Example   string         `json:"example_context,omitempty"`
}

// Priority ranks a group for review: severity first, then failure signals, then what kind of
// event it is. It orders a screening list and claims nothing about cause.
func (g Group) Priority() int {
	switch g.Severity {
	case "F", "E":
		return 100
	case "W":
		return 80
	}
	lower := strings.ToLower(g.Message)
	for _, signal := range []string{"fail", "error", "timeout", "timed out", "unreachable", "refused"} {
		if strings.Contains(lower, signal) {
			return 70
		}
	}
	switch g.Category {
	case Election, InitialSync, Recovery:
		return 60
	case Catalogue:
		return 50
	case Sharding, Replication:
		return 40
	case Other:
		return 30
	}
	return 10
}

// Grouper folds a stream of records into groups.
type Grouper struct {
	groups map[string]*Group
	// shapes caches the fingerprint of each distinct message. A log line's message is a fixed
	// string per log statement — everything that varies is in the attributes — so a bundle holds a
	// few hundred of them and millions of records. Without the cache the grouper runs a regular
	// expression over every line of every file, which was the most expensive thing in a scan.
	shapes map[string]string
}

func NewGrouper() *Grouper {
	return &Grouper{groups: map[string]*Group{}, shapes: map[string]string{}}
}

// Add counts one record.
func (g *Grouper) Add(record Record) {
	fingerprint, cached := g.shapes[record.Msg]
	if !cached {
		fingerprint = Fingerprint(record.Msg)
		g.shapes[record.Msg] = fingerprint
	}
	id := groupID(record.Comp, record.Sev, record.LogID, fingerprint)
	group, seen := g.groups[id]
	if !seen {
		group = &Group{
			ID: id, Category: Classify(record), Severity: record.Sev, Component: record.Comp,
			MessageID: record.LogID, Message: fingerprint, First: record.At, Sources: map[string]int{},
			Example: record.Ctx,
		}
		g.groups[id] = group
	}
	group.Count++
	group.Sources[record.Source]++
	if record.At.Before(group.First) {
		group.First = record.At
	}
	if record.At.After(group.Last) {
		group.Last = record.At
	}
}

// Categories names every bucket a record can fall into, so a caller can offer them without
// guessing at the list.
func Categories() []Category {
	return []Category{SlowQuery, Election, InitialSync, Recovery, Replication, Sharding, Catalogue,
		Storage, Network, Authentication, Query, Other}
}

// Groups returns the groups worth reviewing first.
func (g *Grouper) Groups() []Group {
	out := make([]Group, 0, len(g.groups))
	for _, group := range g.groups {
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := out[i].Priority(), out[j].Priority()
		if left != right {
			return left > right
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// dynamicText is the part of a message that varies between otherwise identical lines: ids, uuids,
// addresses and long numbers. Folding them together is what turns a log into a handful of groups.
var dynamicText = regexp.MustCompile(`\b([0-9a-f]{24,}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|\d+\.\d+\.\d+\.\d+(:\d+)?|\d{4,})\b`)

// Fingerprint reduces a message to its stable shape.
func Fingerprint(message string) string {
	return dynamicText.ReplaceAllString(message, "<value>")
}

func groupID(component, severity string, messageID int, fingerprint string) string {
	sum := sha1.Sum([]byte(component + "\x00" + severity + "\x00" + string(rune(messageID)) + "\x00" + fingerprint))
	return hex.EncodeToString(sum[:6])
}

// Classify buckets a record by component and message text.
func Classify(record Record) Category {
	text := strings.ToLower(record.Comp + " " + record.Msg)
	switch {
	case strings.Contains(text, "slow query"):
		return SlowQuery
	// A catalogue change is read before anything else it mentions. "Index build: scanning
	// collection" says "index" and "collection" and would otherwise land in storage or query,
	// where nothing would ever look for it.
	case strings.HasPrefix(record.Msg, "CMD: ") || strings.Contains(text, "index build") ||
		strings.Contains(text, "createcollection") || strings.Contains(text, "dropcollection") ||
		strings.Contains(text, "dropdatabase") || strings.Contains(text, "renamecollection") ||
		strings.Contains(text, "collmod") || strings.Contains(text, "featurecompatibilityversion"):
		return Catalogue
	// "primary" alone is not an election: a secondary names its primary in ordinary notes, and
	// filing those under election made the category mean nothing.
	case strings.Contains(text, "election") || strings.Contains(text, "stepdown") ||
		strings.Contains(text, "step up") || strings.Contains(text, "stepping down") ||
		strings.Contains(text, "transition to primary") || strings.Contains(text, "no primary"):
		return Election
	case strings.Contains(text, "initial sync"):
		return InitialSync
	case strings.Contains(text, "rollback") || strings.Contains(text, "recover"):
		return Recovery
	case strings.Contains(text, "repl"):
		return Replication
	case strings.Contains(text, "shard") || strings.Contains(text, "chunk") || strings.Contains(text, "balancer"):
		return Sharding
	case strings.Contains(text, "storage") || strings.Contains(text, "wiredtiger") || strings.Contains(text, "checkpoint"):
		return Storage
	case strings.Contains(text, "network") || strings.Contains(text, "connection"):
		return Network
	case strings.Contains(text, "auth"):
		return Authentication
	case strings.Contains(text, "query") || strings.Contains(text, "command"):
		return Query
	}
	return Other
}

// Filtered keeps only the records of one category, which is how a scan narrows without a second pass.
type Filtered struct {
	*Grouper
	category string
}

// Only wraps this grouper so it ignores everything outside the named category; an empty name keeps
// everything.
func (g *Grouper) Only(category string) *Filtered { return &Filtered{Grouper: g, category: category} }

func (f *Filtered) Add(record Record) {
	if f.category == "" || string(Classify(record)) == f.category {
		f.Grouper.Add(record)
	}
}

// Sample keeps the first records of one group, so a reader can see what the lines actually said
// rather than only how often they were said.
type Sample struct {
	ID      string
	limit   int
	Records []Record
}

// SampleOf collects up to limit records belonging to a group id.
func SampleOf(id string, limit int) *Sample { return &Sample{ID: id, limit: limit} }

func (s *Sample) Add(record Record) {
	if len(s.Records) >= s.limit || GroupIDOf(record) != s.ID {
		return
	}
	s.Records = append(s.Records, record)
}

// GroupIDOf is the id the grouper would file this record under.
func GroupIDOf(record Record) string {
	return groupID(record.Comp, record.Sev, record.LogID, Fingerprint(record.Msg))
}
