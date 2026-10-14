// Package guard is the PreToolUse hook that keeps an agent reading a bundle through the parser:
// a file or shell tool that touches a bundle is put to the user (Claude Code) or refused (Codex,
// whose hooks cannot ask), so every number in an answer comes from a mongo_* tool unless the user
// chose otherwise.
package guard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mongo-log-parser/internal/store"
)

type payload struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	Cwd       string          `json:"cwd"`
	// Only Codex sends a turn id; it is how the two hosts are told apart.
	TurnID *string `json:"turn_id"`
}

type toolInput struct {
	FilePath string `json:"file_path"`
	Path     string `json:"path"`
	Command  string `json:"command"`
}

// Run reads one hook payload and answers it. It never fails the tool call on its own trouble: a
// guard that cannot read its input lets the call through.
func Run(stdin io.Reader, stdout, stderr io.Writer) int {
	if os.Getenv("MONGO_LOG_PARSER_RAW_READS") == "1" {
		return 0
	}
	var p payload
	if err := json.NewDecoder(stdin).Decode(&p); err != nil {
		return 0
	}
	var in toolInput
	_ = json.Unmarshal(p.ToolInput, &in)
	hit := touches(p.ToolName, in, p.Cwd, store.Bundles())
	if hit == "" {
		return 0
	}
	reason := fmt.Sprintf("%s reads the MongoDB bundle at %s directly. Answers should come from the mongo_* tools; "+
		"read raw bundle files only when the user explicitly asks.", p.ToolName, hit)
	if p.TurnID != nil {
		fmt.Fprintln(stderr, reason+" Set MONGO_LOG_PARSER_RAW_READS=1 for the session to allow it.")
		return 2
	}
	answer, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "ask",
		"permissionDecisionReason": reason,
	}})
	fmt.Fprintln(stdout, string(answer))
	return 0
}

// shapes are names only a bundle has, for a bundle no store has been built from yet.
var shapes = regexp.MustCompile(`(^|[/\s"'])(diagnostic\.data|mongo[ds][\w.-]*\.log[\w.-]*)($|[/\s"'])`)

// touches is the bundle the call would read, or "".
func touches(tool string, in toolInput, cwd string, bundles []string) string {
	switch tool {
	case "Read", "Grep", "Glob":
		target := in.FilePath
		if target == "" {
			target = in.Path
		}
		if target == "" {
			target = cwd
		} else if !filepath.IsAbs(target) {
			target = filepath.Join(cwd, target)
		}
		return within(target, bundles)
	case "Bash":
		if parserOnly(in.Command) {
			return ""
		}
		for _, root := range bundles {
			if strings.Contains(in.Command, root) {
				return root
			}
		}
		if root := within(cwd, bundles); root != "" {
			return root
		}
		if match := shapes.FindStringSubmatch(in.Command); match != nil {
			return match[2]
		}
	}
	return ""
}

// within is the bundle path sits in, or a bundle-shaped part of it.
func within(path string, bundles []string) string {
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	for _, root := range bundles {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return root
		}
	}
	if match := shapes.FindStringSubmatch(path); match != nil {
		return path
	}
	return ""
}

// parserOnly is a command that runs the parser and nothing else, which is the sanctioned way in.
func parserOnly(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 || filepath.Base(fields[0]) != "mongo-log-parser" {
		return false
	}
	return !strings.ContainsAny(command, ";|&`$<>\n")
}
