package copilot

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest/reachtest"
)

const reachSession = "copilot-reach-session"

func copilotCall(m *reachtest.Machine, cwd, tool string, input map[string]any) {
	m.T.Helper()
	m.Run(postToolUse, reachSession, name, cwd, map[string]any{"session_id": reachSession, "cwd": cwd, "tool_name": tool,
		"tool_input": input, "tool_result": map[string]any{"result_type": "success"}})
}

// A personal session whose read, search or shell command reaches Acme's checkout has
// crossed: Copilot's PostToolUse, which fires for every tool, classifies what the call
// reached, and the run turns Mixed. Reaching its own checkout does not.
func TestRoutedCopilotReadsAndShellCommandsReachingAcmeMakeTheRunMixed(t *testing.T) {
	for tool, input := range map[string]func(m *reachtest.Machine, root string) map[string]any{
		"view": func(_ *reachtest.Machine, root string) map[string]any {
			return map[string]any{"path": filepath.Join(root, "README.md")}
		},
		"read_file": func(_ *reachtest.Machine, root string) map[string]any {
			return map[string]any{"filePath": filepath.Join(root, "README.md")}
		},
		"list_dir": func(_ *reachtest.Machine, root string) map[string]any { return map[string]any{"path": root} },
		"grep": func(_ *reachtest.Machine, root string) map[string]any {
			return map[string]any{"pattern": "TODO", "path": root}
		},
		"bash": func(m *reachtest.Machine, root string) map[string]any {
			return map[string]any{"command": "cat " + filepath.Join(root, "README.md")}
		},
		"run_in_terminal": func(m *reachtest.Machine, root string) map[string]any {
			return map[string]any{"command": "ls " + root}
		},
	} {
		t.Run(tool, func(t *testing.T) {
			m := reachtest.New(t)
			m.Run(sessionStart, reachSession, name, m.Side, map[string]any{"session_id": reachSession, "cwd": m.Side})
			copilotCall(m, m.Side, tool, input(m, m.Side))
			if m.Mixed(reachSession) {
				t.Fatal("Mixed after reaching only its own checkout")
			}
			copilotCall(m, m.Side, tool, input(m, m.API))
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
func TestRoutedCopilotReadInADroppedFolderAndUnmigrated(t *testing.T) {
	m := reachtest.New(t)
	outside := m.Outside()
	m.Run(sessionStart, reachSession, name, outside, map[string]any{"session_id": reachSession, "cwd": outside})
	copilotCall(m, outside, "view", map[string]any{"path": filepath.Join(m.API, "NOTES.md")})
	if m.Mixed(reachSession) || !slices.Contains(m.Locations(reachSession), "github.com/acme/api") {
		t.Fatalf("Mixed %v, the run ledger records %v", m.Mixed(reachSession), m.Locations(reachSession))
	}
	copilotCall(m, m.Side, "view", map[string]any{"path": filepath.Join(m.Side, "README.md")})
	if !m.Mixed(reachSession) {
		t.Fatal("placed in personal's checkout after reaching Acme's, the run is not Mixed")
	}

	old := reachtest.New(t)
	old.Unmigrated = true
	copilotCall(old, old.Side, "view", map[string]any{"path": filepath.Join(old.API, "NOTES.md")})
	copilotCall(old, old.Side, "bash", map[string]any{"command": "cat ../acme-api/NOTES.md"})
	if old.Wrote() {
		t.Fatal("a machine not migrated wrote a run ledger or a placement for a read")
	}
}
