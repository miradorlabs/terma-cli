package gemini

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest/reachtest"
)

const reachSession = "gemini-reach-session"

func geminiCall(m *reachtest.Machine, cwd, tool string, input map[string]any) {
	m.T.Helper()
	m.Run(afterTool, reachSession, geminiTool, cwd, map[string]any{"session_id": reachSession, "cwd": cwd, "tool_name": tool, "tool_input": input})
}

// A personal session whose read, search, listing or shell command reaches Acme's checkout
// has crossed: AfterTool, which matches every tool, classifies what the call reached, and
// the run turns Mixed. Reaching its own checkout does not.
func TestRoutedGeminiReadsAndShellCommandsReachingAcmeMakeTheRunMixed(t *testing.T) {
	for tool, input := range map[string]func(root string) map[string]any{
		"read_file": func(root string) map[string]any {
			return map[string]any{"absolute_path": filepath.Join(root, "README.md")}
		},
		"read_many_files": func(root string) map[string]any {
			return map[string]any{"paths": []string{filepath.Join(root, "README.md")}}
		},
		"list_directory":      func(root string) map[string]any { return map[string]any{"dir_path": root} },
		"glob":                func(root string) map[string]any { return map[string]any{"pattern": "**/*.md", "path": root} },
		"search_file_content": func(root string) map[string]any { return map[string]any{"pattern": "TODO", "dir_path": root} },
		"run_shell_command":   func(root string) map[string]any { return map[string]any{"command": "cat README.md", "directory": root} },
	} {
		t.Run(tool, func(t *testing.T) {
			m := reachtest.New(t)
			m.Run(sessionStart, reachSession, geminiTool, m.Side, map[string]any{"session_id": reachSession, "cwd": m.Side})
			geminiCall(m, m.Side, tool, input(m.Side))
			if m.Mixed(reachSession) {
				t.Fatal("Mixed after reaching only its own checkout")
			}
			geminiCall(m, m.Side, tool, input(m.API))
			if !m.Mixed(reachSession) {
				t.Fatal("not Mixed after reaching Acme's checkout")
			}
		})
	}
}

// A session whose own folder Route drops, outside git, sends nothing, but still records in
// the run ledger the Acme checkout a read reached, so placed in personal's checkout later
// the run has crossed. On a machine not migrated a read classifies nothing and writes
// nothing.
func TestRoutedGeminiReadInADroppedFolderAndUnmigrated(t *testing.T) {
	m := reachtest.New(t)
	outside := m.Outside()
	m.Run(sessionStart, reachSession, geminiTool, outside, map[string]any{"session_id": reachSession, "cwd": outside})
	geminiCall(m, outside, "read_file", map[string]any{"file_path": filepath.Join(m.API, "NOTES.md")})
	if m.Mixed(reachSession) || !slices.Contains(m.Locations(reachSession), "github.com/acme/api") {
		t.Fatalf("Mixed %v, the run ledger records %v", m.Mixed(reachSession), m.Locations(reachSession))
	}
	geminiCall(m, m.Side, "read_file", map[string]any{"file_path": filepath.Join(m.Side, "README.md")})
	if !m.Mixed(reachSession) {
		t.Fatal("placed in personal's checkout after reaching Acme's, the run is not Mixed")
	}

	old := reachtest.New(t)
	old.Unmigrated = true
	geminiCall(old, old.Side, "read_file", map[string]any{"file_path": filepath.Join(old.API, "NOTES.md")})
	geminiCall(old, old.Side, "run_shell_command", map[string]any{"command": "cat ../acme-api/NOTES.md"})
	if old.Wrote() {
		t.Fatal("a machine not migrated wrote a run ledger or a placement for a read")
	}
}
