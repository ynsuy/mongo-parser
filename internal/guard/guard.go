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

	"github.com/ynsuy/mongo-parser/internal/store"
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
	if os.Getenv("MONGO_PARSER_RAW_READS") == "1" {
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
		fmt.Fprintln(stderr, reason+" Set MONGO_PARSER_RAW_READS=1 for the session to allow it.")
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

// searches is a command that reads every file under a directory.
var searches = regexp.MustCompile(`(^|[\s;|&(])((rg|ag|ack)(\s|$)|[ez]?grep\s([^|;&]*\s)?(-[a-zA-Z]*[rR]|--recursive)|find\s[^|;&]*-exec)`)

// touches is the bundle the call would read, or "". Only reading a file the parser reads counts:
// listing names (Glob, ls) and the user's own files beside the bundle do not.
func touches(tool string, in toolInput, cwd string, bundles []store.Bundle) string {
	switch tool {
	case "Read", "Grep":
		target := in.FilePath
		if target == "" {
			target = in.Path
		}
		if target == "" {
			target = cwd
		} else if !filepath.IsAbs(target) {
			target = filepath.Join(cwd, target)
		}
		if root := reads(filepath.Clean(target), bundles, tool == "Grep"); root != "" {
			return root
		}
		return shaped(target)
	case "Bash":
		if parserOnly(in.Command) {
			return ""
		}
		search := searches.MatchString(in.Command)
		if search && cwd != "" {
			if root := reads(filepath.Clean(cwd), bundles, true); root != "" {
				return root
			}
		}
		for _, word := range words(in.Command) {
			if strings.HasPrefix(word, "~/") {
				if home, err := os.UserHomeDir(); err == nil {
					word = filepath.Join(home, word[2:])
				}
			}
			if !filepath.IsAbs(word) {
				word = filepath.Join(cwd, word)
			}
			if root := reads(filepath.Clean(word), bundles, search); root != "" {
				return root
			}
		}
		return shaped(in.Command)
	}
	return ""
}

// reads is the bundle whose file path is, or, for a search, whose files path holds.
func reads(path string, bundles []store.Bundle, search bool) string {
	sep := string(filepath.Separator)
	for _, b := range bundles {
		if b.Files == nil {
			if path == b.Root || strings.HasPrefix(path, b.Root+sep) || search && strings.HasPrefix(b.Root, path+sep) {
				return b.Root
			}
			continue
		}
		for _, file := range b.Files {
			if path == file || search && strings.HasPrefix(file, path+sep) {
				return b.Root
			}
		}
	}
	return ""
}

func shaped(text string) string {
	if match := shapes.FindStringSubmatch(text); match != nil {
		return match[2]
	}
	return ""
}

// words splits a command the way a shell would, enough to find the paths in it: quotes kept
// together, operators as separators, and the value of a --flag=path.
func words(command string) []string {
	var out []string
	var word strings.Builder
	var quote rune
	flush := func() {
		if word.Len() > 0 {
			w := word.String()
			if i := strings.Index(w, "="); i >= 0 && strings.HasPrefix(w, "-") {
				w = w[i+1:]
			}
			if w != "" && !strings.HasPrefix(w, "-") {
				out = append(out, w)
			}
			word.Reset()
		}
	}
	for _, r := range command {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			word.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case strings.ContainsRune(" \t\n;|&<>()`", r):
			flush()
		default:
			word.WriteRune(r)
		}
	}
	flush()
	return out
}

// parserOnly is a command that runs the parser and nothing else, which is the sanctioned way in.
func parserOnly(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 || filepath.Base(fields[0]) != "mongo-parser" {
		return false
	}
	return !strings.ContainsAny(command, ";|&`$<>\n")
}
