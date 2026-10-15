package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ynsuy/mongo-parser/internal/render"
	"github.com/ynsuy/mongo-parser/internal/shell"
	"github.com/ynsuy/mongo-parser/internal/store"
)

// A collection script runs a handful of shell commands and keeps their printouts beside the logs:
// serverStatus(), rs.status(), hostInfo(), getCmdLineOpts(). Each is a single moment rather than a
// history, and each states outright something no log line ever says — the connection ceiling, the
// members' health, how much memory the machine has, what the process was started with.
//
// They are read out of the store, not off the disk. The bundle is walked once, at ingest, and a
// printout inside an archive never had a path to walk to.

// runShell prints what the bundle's shell printouts say, or lists them when none is named.
func runShell(root string, args []string) error {
	flags, c := newFlags("shell")
	color := flags.String("color", "never", "never (default), auto or always")
	words, err := parse(flags, args)
	if err != nil {
		return err
	}
	st, err := openStore(root, c.refresh)
	if err != nil {
		return err
	}
	defer st.Close()

	held := shellArtifacts(st)
	if len(held) == 0 {
		return emit(os.Stdout, c.json, "shell", root, map[string]any{"outputs": []string{}},
			withVersion("this bundle holds no shell output: nothing in it is a printout of serverStatus(), rs.status() or the like.\nWhat the bundle does hold is in: coverage"))
	}
	wanted := strings.ToLower(strings.Join(words, " "))
	options := render.TableOptions{Width: outputWidth(c.width), Color: useColor(*color) && !c.json}

	paths := make([]string, 0, len(held))
	for path := range held {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	matched := make([]string, 0, len(paths))
	for _, path := range paths {
		if wanted == "" || strings.Contains(strings.ToLower(path), wanted) || strings.Contains(strings.ToLower(held[path].kind), wanted) {
			matched = append(matched, path)
		}
	}
	if len(matched) == 0 {
		return fmt.Errorf("no shell output here is called %s; this bundle holds:\n  %s", wanted, strings.Join(paths, "\n  "))
	}
	// Naming nothing lists what there is rather than printing a dozen whole documents: the next
	// question is always about one of them, and the list is what says which names it takes.
	if wanted == "" && len(matched) > 1 {
		rows := make([]string, len(matched))
		kinds := make([]string, len(matched))
		for i, path := range matched {
			rows[i] = path
			kinds[i] = held[path].kind
		}
		table := render.Table([]render.Column{
			{Title: "Output", Values: rows, Elastic: true},
			{Title: "Kind", Values: kinds},
		}, options)
		return emit(os.Stdout, c.json, "shell", root, map[string]any{"outputs": matched},
			withVersion(table+"\n\nname one of them to read it: mongo-parser shell "+quoteArg(root)+" rs_status"))
	}

	parts := make([]string, 0, len(matched))
	for _, path := range matched {
		parts = append(parts, section(path, shellBody(held[path], options)))
	}
	return emit(os.Stdout, c.json, "shell", root, map[string]any{"outputs": matched},
		withVersion(strings.Join(parts, "\n\n")))
}

// artifact is one stored printout: what the store decided it was, and what it said.
type artifact struct {
	kind    string
	content string
}

// shellArtifacts is every shell printout the store kept. The store files them as
// shell_output/<command>, so the kind is the command and not a guess from the file name.
func shellArtifacts(st *store.Store) map[string]artifact {
	rows, err := st.DB.Query(`
		SELECT source.path, artifact.kind, artifact.content FROM artifact JOIN source ON source.id = artifact.source
		WHERE artifact.kind = 'shell_output' OR artifact.kind LIKE 'shell_output/%'`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	held := map[string]artifact{}
	for rows.Next() {
		var path, kind, content string
		if err := rows.Scan(&path, &kind, &content); err != nil {
			return held
		}
		_, command, found := strings.Cut(kind, "/")
		if !found {
			command = "unknown"
		}
		held[path] = artifact{kind: command, content: content}
	}
	return held
}

// shellBody renders one printout by what it is. A command this build has no reader for is still
// worth answering — the document itself is the answer — so it is printed as the shell wrote it.
func shellBody(held artifact, options render.TableOptions) string {
	switch held.kind {
	case "serverstatus":
		return serverStatusBody(held.content)
	case "rs_status", "replsetgetstatus":
		return replicaSetStatusBody(held.content, options)
	case "host_info", "hostinfo":
		return hostInfoBody(held.content)
	case "getcmdlineopts":
		return commandLineBody(held.content)
	}
	return strings.TrimRight(held.content, "\n")
}

// serverStatusBody is the handful of figures a triage reads off serverStatus first: what the
// process is, how close its connections are to the ceiling, and what it has in memory.
func serverStatusBody(content string) string {
	info, err := shell.ParseServerStatus(content)
	if err != nil {
		return "this serverStatus could not be read: " + err.Error()
	}
	lines := []string{fmt.Sprintf("%s · MongoDB %s · %s · pid %d · up %s",
		orDash(info.Host), orDash(info.Version), orDash(info.Process), info.PID, duration(int64(info.Uptime)*1000))}
	if len(info.Service) > 0 {
		lines = append(lines, "services: "+strings.Join(info.Service, ", "))
	}

	// The ceiling matters more than the count: 2,000 connections is nothing on a 50,000 limit and
	// the end of the deployment on a 2,048 one, and only the pair says which.
	ceiling := info.Connections.Current + info.Connections.Available
	connections := fmt.Sprintf("%s in use", plural(info.Connections.Current, "connection"))
	if ceiling > 0 {
		connections += fmt.Sprintf(" of %d (%.0f%%), %s opened since startup",
			ceiling, float64(info.Connections.Current)/float64(ceiling)*100, thousands(info.Connections.TotalCreated))
	}
	if info.Connections.Rejected > 0 {
		connections += fmt.Sprintf(", %s refused", plural(info.Connections.Rejected, "connection"))
	}
	lines = append(lines, connections)

	if info.Memory.Resident > 0 {
		// serverStatus reports these in megabytes, which is the one unit nothing else here uses.
		lines = append(lines, fmt.Sprintf("memory: %s resident, %s virtual",
			bytesText(info.Memory.Resident<<20), bytesText(info.Memory.Virtual<<20)))
	}
	if info.Replication.Members > 0 {
		role := "neither primary nor secondary"
		switch {
		case info.Replication.IsMaster:
			role = "PRIMARY"
		case info.Replication.IsSecondary:
			role = "SECONDARY"
		case info.Replication.IsArbiter:
			role = "ARBITER"
		}
		lines = append(lines, fmt.Sprintf("replication: %s of %s, state %d",
			role, plural(info.Replication.Members, "member"), info.Replication.MyState))
	}
	if info.Network.NumRequests > 0 {
		lines = append(lines, fmt.Sprintf("network: %s in, %s out over %s requests since startup",
			bytesText(info.Network.BytesIn), bytesText(info.Network.BytesOut), thousands(info.Network.NumRequests)))
	}
	return strings.Join(lines, "\n")
}

// replicaSetStatusBody is what rs.status() says about every member, which is the one statement in a
// bundle of a set's health at a single moment: a log says what each node thought of itself.
func replicaSetStatusBody(content string, options render.TableOptions) string {
	data, err := shell.ParseEJSON(content)
	if err != nil {
		return "this rs.status() could not be read: " + err.Error()
	}
	members := shell.GetArray(data, "members")
	heading := fmt.Sprintf("replica set %s, term %d, primary %s",
		orDash(shell.GetString(data, "set")), shell.GetInt(data, "term"), orDash(shell.GetString(data, "primary")))
	if len(members) == 0 {
		return heading + "\nno members in this document"
	}
	names := make([]string, 0, len(members))
	states := make([]string, 0, len(members))
	healths := make([]string, 0, len(members))
	for _, entry := range members {
		member, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		names = append(names, shell.GetString(member, "name"))
		state := shell.GetString(member, "stateStr")
		if state == "" {
			state = memberState(shell.GetInt(member, "state"))
		}
		states = append(states, state)
		// health is 1 or 0 and nothing else; the word is what a reader acts on.
		if shell.GetInt(member, "health") == 1 {
			healths = append(healths, "reachable")
		} else {
			healths = append(healths, "unreachable")
		}
	}
	return heading + "\n" + render.Table([]render.Column{
		{Title: "Member", Align: render.AlignTail, Values: names, Elastic: true},
		{Title: "State", Values: states},
		{Title: "Health", Values: healths},
	}, options)
}

// memberState names the numeric state, for a document that carried no stateStr.
func memberState(state int) string {
	states := map[int]string{
		0: "STARTUP", 1: "PRIMARY", 2: "SECONDARY", 3: "RECOVERING", 4: "FATAL", 5: "STARTUP2",
		6: "UNKNOWN", 7: "ARBITER", 8: "DOWN", 9: "ROLLBACK", 10: "REMOVED",
	}
	if name, ok := states[state]; ok {
		return name
	}
	return fmt.Sprintf("state %d", state)
}

// hostInfoBody is the machine under the server, which decides how every figure above it reads: a
// queue depth means one thing on 16 cores and another on 96.
func hostInfoBody(content string) string {
	data, err := shell.ParseEJSON(content)
	if err != nil {
		return "this hostInfo could not be read: " + err.Error()
	}
	lines := make([]string, 0, 3)
	if system := shell.GetMap(data, "system"); system != nil {
		// os sits beside system in the document rather than inside it, and the architecture is
		// spelled cpuArch: reading them from the wrong place printed three dashes and a core count.
		lines = append(lines, fmt.Sprintf("%s · %s %s · %s · %s",
			orDash(shell.GetString(system, "hostname")),
			orDash(shell.GetString(data, "os", "type")), orDash(shell.GetString(data, "os", "name")),
			orDash(shell.GetString(system, "cpuArch")),
			plural(shell.GetInt(system, "numCores"), "core")))
		if memory := shell.GetInt(system, "memSizeMB"); memory > 0 {
			lines = append(lines, "memory: "+bytesText(int64(memory)<<20))
		}
	}
	if extra := shell.GetMap(data, "extra"); extra != nil {
		if pages := shell.GetString(extra, "transparentHugePages"); pages != "" {
			// One of the two OS settings a MongoDB support case turns on more often than any other.
			lines = append(lines, "transparent huge pages: "+pages)
		}
	}
	if len(lines) == 0 {
		return strings.TrimRight(content, "\n")
	}
	return strings.Join(lines, "\n")
}

// commandLineBody is what the process was started with, which is the only statement of the
// configuration that was actually in force rather than the one in a file on disk.
func commandLineBody(content string) string {
	data, err := shell.ParseEJSON(content)
	if err != nil {
		return "this getCmdLineOpts could not be read: " + err.Error()
	}
	parsed := shell.GetMap(data, "parsed")
	if parsed == nil {
		return strings.TrimRight(content, "\n")
	}
	settings := make([]string, 0, 32)
	flattenWhole("", parsed, &settings)
	sort.Strings(settings)
	return strings.Join(settings, "\n")
}
