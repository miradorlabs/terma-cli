package antigravity

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest/reachtest"
)

const reachSession = "agy-reach-conversation"

func agyStart(m *reachtest.Machine, cwd string) {
	m.T.Helper()
	m.Run(preInvocation, reachSession, antigravityTool, cwd, map[string]any{"conversationId": reachSession, "workspacePaths": []string{cwd},
		"invocationNum": 0, "initialNumSteps": 0})
}

func agyStep(m *reachtest.Machine, cwd, tool string, args map[string]any) {
	m.T.Helper()
	m.Run(postToolUse, reachSession, antigravityTool, cwd, map[string]any{"conversationId": reachSession, "workspacePaths": []string{cwd},
		"stepIdx": 3, "toolCall": map[string]any{"name": tool, "args": args}})
}

// A personal conversation whose read, search or shell command reaches Acme's checkout has
// crossed: PostToolUse, which fires on every step, classifies what the step reached, and
// the run turns Mixed. Reaching its own checkout does not.
func TestRoutedAntigravityReadsAndShellCommandsReachingAcmeMakeTheRunMixed(t *testing.T) {
	for tool, args := range map[string]func(root string) map[string]any{
		"view_file": func(root string) map[string]any {
			return map[string]any{"AbsolutePath": filepath.Join(root, "README.md")}
		},
		"list_dir":     func(root string) map[string]any { return map[string]any{"DirectoryPath": root} },
		"grep_search":  func(root string) map[string]any { return map[string]any{"Query": "TODO", "SearchPath": root} },
		"find_by_name": func(root string) map[string]any { return map[string]any{"Pattern": "*.md", "SearchDirectory": root} },
		"run_command":  func(root string) map[string]any { return map[string]any{"CommandLine": "cat README.md", "Cwd": root} },
		"str_replace_editor": func(root string) map[string]any {
			return map[string]any{"command": "view", "path": filepath.Join(root, "README.md")}
		},
	} {
		t.Run(tool, func(t *testing.T) {
			m := reachtest.New(t)
			agyStart(m, m.Side)
			agyStep(m, m.Side, tool, args(m.Side))
			if m.Mixed(reachSession) {
				t.Fatal("Mixed after reaching only its own checkout")
			}
			agyStep(m, m.Side, tool, args(m.API))
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
func TestRoutedAntigravityReadInADroppedFolderAndUnmigrated(t *testing.T) {
	m := reachtest.New(t)
	outside := m.Outside()
	agyStart(m, outside)
	agyStep(m, outside, "view_file", map[string]any{"AbsolutePath": filepath.Join(m.API, "NOTES.md")})
	if m.Mixed(reachSession) || !slices.Contains(m.Locations(reachSession), "github.com/acme/api") {
		t.Fatalf("Mixed %v, the run ledger records %v", m.Mixed(reachSession), m.Locations(reachSession))
	}
	agyStep(m, m.Side, "view_file", map[string]any{"AbsolutePath": filepath.Join(m.Side, "README.md")})
	if !m.Mixed(reachSession) {
		t.Fatal("placed in personal's checkout after reaching Acme's, the run is not Mixed")
	}

	old := reachtest.New(t)
	old.Unmigrated = true
	agyStep(old, old.Side, "view_file", map[string]any{"AbsolutePath": filepath.Join(old.API, "NOTES.md")})
	agyStep(old, old.Side, "run_command", map[string]any{"CommandLine": "cat ../acme-api/NOTES.md"})
	if old.Wrote() {
		t.Fatal("a machine not migrated wrote a run ledger or a placement for a read")
	}
}
