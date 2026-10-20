package guard

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ynsuy/mongo-parser/internal/store"
)

func TestTouches(t *testing.T) {
	bundles := []store.Bundle{
		{Root: "/cases/CS1", Files: []string{"/cases/CS1/node1/a.log", "/cases/CS1/node1/b c.log", "/cases/CS1/logs.tar.gz"}},
		{Root: "/cases/CS2"},
	}
	cases := []struct {
		tool, path, command, cwd, want string
	}{
		{tool: "Read", path: "/cases/CS1/node1/a.log", cwd: "/work", want: "/cases/CS1"},
		{tool: "Read", path: "node1/a.log", cwd: "/cases/CS1", want: "/cases/CS1"},
		{tool: "Read", path: "/cases/CS1/notes.md", cwd: "/cases/CS1", want: ""},
		{tool: "Read", path: "/cases/CS1/node1/mongod.log", cwd: "/work", want: "mongod.log"},
		{tool: "Read", path: "/cases/CS2/anything", cwd: "/work", want: "/cases/CS2"},
		{tool: "Read", path: "/work/main.go", cwd: "/work", want: ""},
		{tool: "Grep", cwd: "/cases/CS1", want: "/cases/CS1"},
		{tool: "Grep", path: "/cases", cwd: "/work", want: "/cases/CS1"},
		{tool: "Grep", path: "/cases/CS1/notes", cwd: "/work", want: ""},
		{tool: "Glob", path: "/cases/CS1", cwd: "/cases/CS1", want: ""},
		{tool: "Bash", command: "ls", cwd: "/cases/CS1/node1", want: ""},
		{tool: "Bash", command: "git status && cat notes.md", cwd: "/cases/CS1", want: ""},
		{tool: "Bash", command: "grep -c ERROR /cases/CS1/node1/a.log", cwd: "/work", want: "/cases/CS1"},
		{tool: "Bash", command: "tail -n 5 node1/a.log", cwd: "/cases/CS1", want: "/cases/CS1"},
		{tool: "Bash", command: `head "b c.log"`, cwd: "/cases/CS1/node1", want: "/cases/CS1"},
		{tool: "Bash", command: "tar -tzf logs.tar.gz | head", cwd: "/cases/CS1", want: "/cases/CS1"},
		{tool: "Bash", command: "grep -rn timeout .", cwd: "/cases/CS1", want: "/cases/CS1"},
		{tool: "Bash", command: "rg timeout", cwd: "/cases/CS1", want: "/cases/CS1"},
		{tool: "Bash", command: "rg timeout", cwd: "/work", want: ""},
		{tool: "Bash", command: "mongo-parser slow /cases/CS1 --top 5", cwd: "/work", want: ""},
		{tool: "Bash", command: "mongo-parser slow /cases/CS1 | head; cat /cases/CS1/node1/a.log", cwd: "/work", want: "/cases/CS1"},
		{tool: "Bash", command: "zcat ~/new/diagnostic.data/metrics.1", cwd: "/work", want: "diagnostic.data"},
		{tool: "Bash", command: "tail -n 50 logs/mongod.log.2", cwd: "/work", want: "mongod.log.2"},
		{tool: "Bash", command: "go test ./...", cwd: "/work", want: ""},
		{tool: "Write", path: "/cases/CS1/notes.md", cwd: "/work", want: ""},
	}
	for _, c := range cases {
		in := toolInput{FilePath: c.path, Command: c.command}
		if c.tool == "Grep" || c.tool == "Glob" {
			in = toolInput{Path: c.path}
		}
		if got := touches(c.tool, in, c.cwd, bundles); got != c.want {
			t.Errorf("%s %q %q in %s: got %q, want %q", c.tool, c.path, c.command, c.cwd, got, c.want)
		}
	}
}

func TestRunAsksOnClaudeAndRefusesOnCodex(t *testing.T) {
	t.Setenv("MONGO_PARSER_CACHE", t.TempDir())
	var out, errs bytes.Buffer
	claude := `{"tool_name":"Bash","tool_input":{"command":"cat x/diagnostic.data/m"},"cwd":"/w"}`
	if code := Run(strings.NewReader(claude), &out, &errs); code != 0 || !strings.Contains(out.String(), `"permissionDecision":"ask"`) {
		t.Fatalf("claude: code %d out %q", code, out.String())
	}
	out.Reset()
	codex := `{"tool_name":"Bash","tool_input":{"command":"cat x/diagnostic.data/m"},"cwd":"/w","turn_id":"t1"}`
	if code := Run(strings.NewReader(codex), &out, &errs); code != 2 || out.Len() != 0 || errs.Len() == 0 {
		t.Fatalf("codex: code %d out %q err %q", code, out.String(), errs.String())
	}
	t.Setenv("MONGO_PARSER_RAW_READS", "1")
	out.Reset()
	if code := Run(strings.NewReader(codex), &out, &errs); code != 0 || out.Len() != 0 {
		t.Fatalf("override: code %d out %q", code, out.String())
	}
}
