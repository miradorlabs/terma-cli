package cursor

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest/reachtest"
)

const reachSession = "cursor-reach-conversation"

func cursorCall(m *reachtest.Machine, cwd, tool string, input map[string]any) {
	m.T.Helper()
	m.Run(postToolUse, reachSession, cursorTool, cwd, map[string]any{"conversation_id": reachSession, "hook_event_name": "postToolUse",
		"workspace_roots": []string{cwd}, "tool_name": tool, "tool_use_id": "call-1", "tool_input": input})
}

// A personal conversation whose read, search or shell command reaches Acme's checkout has
// crossed: postToolUse classifies what the call reached, and the run turns Mixed. Reaching
// its own checkout does not.
func TestRoutedCursorReadsAndShellCommandsReachingAcmeMakeTheRunMixed(t *testing.T) {
	for tool, input := range map[string]func(root string) map[string]any{
		"Read": func(root string) map[string]any {
			return map[string]any{"target_file": filepath.Join(root, "README.md")}
		},
		"Grep":  func(root string) map[string]any { return map[string]any{"pattern": "TODO", "path": root} },
		"LS":    func(root string) map[string]any { return map[string]any{"target_directory": root} },
		"Shell": func(root string) map[string]any { return map[string]any{"command": "cat README.md", "cwd": root} },
	} {
		t.Run(tool, func(t *testing.T) {
			m := reachtest.New(t)
			m.Run(sessionStart, reachSession, cursorTool, m.Side, map[string]any{"conversation_id": reachSession, "hook_event_name": "sessionStart", "workspace_roots": []string{m.Side}})
			cursorCall(m, m.Side, tool, input(m.Side))
			if m.Mixed(reachSession) {
				t.Fatal("Mixed after reaching only its own checkout")
			}
			cursorCall(m, m.Side, tool, input(m.API))
			if !m.Mixed(reachSession) {
				t.Fatal("not Mixed after reaching Acme's checkout")
			}
		})
	}
}

// A conversation whose own folder Route drops, outside git, sends nothing, but still
// records in the run ledger the Acme checkout a read reached, so placed in personal's
// checkout later the run has crossed. On a machine not migrated a read classifies nothing
// and writes nothing.
func TestRoutedCursorReadInADroppedFolderAndUnmigrated(t *testing.T) {
	m := reachtest.New(t)
	outside := m.Outside()
	m.Run(sessionStart, reachSession, cursorTool, outside, map[string]any{"conversation_id": reachSession, "hook_event_name": "sessionStart", "workspace_roots": []string{outside}})
	cursorCall(m, outside, "Read", map[string]any{"target_file": filepath.Join(m.API, "NOTES.md")})
	if m.Mixed(reachSession) || !slices.Contains(m.Locations(reachSession), "github.com/acme/api") {
		t.Fatalf("Mixed %v, the run ledger records %v", m.Mixed(reachSession), m.Locations(reachSession))
	}
	cursorCall(m, m.Side, "Read", map[string]any{"target_file": filepath.Join(m.Side, "README.md")})
	if !m.Mixed(reachSession) {
		t.Fatal("placed in personal's checkout after reaching Acme's, the run is not Mixed")
	}

	old := reachtest.New(t)
	old.Unmigrated = true
	cursorCall(old, old.Side, "Read", map[string]any{"target_file": filepath.Join(old.API, "NOTES.md")})
	cursorCall(old, old.Side, "Shell", map[string]any{"command": "cat ../acme-api/NOTES.md"})
	if old.Wrote() {
		t.Fatal("a machine not migrated wrote a run ledger or a placement for a read")
	}
}
