//go:build unix

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Drive the commands install actually wrote, with the payload an agent supplies.
func (s *installSandbox) claudeEvent(root, cwd, event, id, file string) {
	s.t.Helper()
	var doc struct {
		Hooks map[string][]struct{ Hooks []struct{ Command string } }
	}
	if err := json.Unmarshal(readInstallFile(s.t, root, hookmgr.ClaudeSettingsPath), &doc); err != nil {
		s.t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"session_id": id, "cwd": cwd, "hook_event_name": event, "tool_name": "Write", "tool_input": map[string]string{"file_path": filepath.Join(root, file)}})
	if err != nil {
		s.t.Fatal(err)
	}
	groups := doc.Hooks[event]
	if len(groups) == 0 || len(groups[0].Hooks) == 0 {
		s.t.Fatalf("missing installed %s hook", event)
	}
	if out, err := s.run(cwd, string(payload), "/bin/sh", "-c", groups[0].Hooks[0].Command); err != nil {
		s.t.Fatalf("installed %s hook: %v\n%s", event, err, out)
	}
}

func TestInstallE2ENonGitToGit(t *testing.T) {
	for _, kind := range []string{"existing_session", "new_session_during_transition", "first_event_after_git_init", "active_without_manifest", "retry_after_rejected_hook"} {
		t.Run(kind, func(t *testing.T) {
			s := newInstallSandbox(t)
			root := s.mkdir("workspace")
			nested := s.mkdir("workspace", "one", "two")
			s.install(root)
			before, err := termaproject.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			originalHooks := readInstallFile(t, root, hookmgr.ClaudeSettingsPath)
			dirs, err := filepath.Glob(filepath.Join(s.base, "config", "workspaces", "*"))
			if err != nil || len(dirs) != 1 {
				t.Fatalf("private store reservation: %v %v", dirs, err)
			}
			private := dirs[0]
			store := session.Open(private)
			activeID := "before-git"
			var files, ids []string
			if kind != "first_event_after_git_init" {
				s.claudeEvent(root, nested, "SessionStart", activeID, "")
				if kind != "active_without_manifest" {
					s.write(root, "before.txt", "agent work before Git\n")
					s.claudeEvent(root, nested, "PostToolUse", activeID, "before.txt")
					files = append(files, "before.txt")
					ids = append(ids, activeID)
				}
			}

			s.git(root, "init", "-q")
			// The root has changed from non-Git to Git, but new events must already use
			// the same store, even before install is run again.
			if kind == "new_session_during_transition" || kind == "first_event_after_git_init" {
				activeID = "after-git"
				s.claudeEvent(root, nested, "SessionStart", activeID, "")
			}
			if kind != "active_without_manifest" {
				s.write(root, "after.txt", "agent work after Git\n")
				s.claudeEvent(root, nested, "PostToolUse", activeID, "after.txt")
				files = append(files, "after.txt")
				if kind == "new_session_during_transition" || kind == "first_event_after_git_init" {
					ids = append(ids, activeID)
				}
			} else {
				// No manifest: preserve the existing active-session fallback as well.
				s.write(root, "agent.txt", "agent work without a file-touch event\n")
				files = []string{"agent.txt"}
				ids = []string{activeID}
			}
			status := s.cli(nested, "status")
			if !strings.Contains(status, activeID) {
				t.Fatalf("active session disappeared immediately after git init:\n%s", status)
			}
			active, _ := store.Active(time.Now(), 0)
			if active == nil || active.ID != activeID {
				t.Fatalf("new hooks switched stores: %+v", active)
			}
			manifests, err := store.Manifests()
			if err != nil {
				t.Fatal(err)
			}
			if kind != "active_without_manifest" {
				recorded := 0
				for _, m := range manifests {
					recorded += len(m.Files)
				}
				if recorded != len(files) {
					t.Fatalf("edits split between stores: %+v", manifests)
				}
			}

			args := []string{"install", "--harness", "none", "--yes", "--no-doctor"}
			s.cli(nested, append(args, "--dry-run")...)
			s.cli(nested, append(args, "--no-hooks")...)
			requireAbsent(t, filepath.Join(root, hookmgr.ShimDir))
			if kind == "retry_after_rejected_hook" {
				s.write(root, hookmgr.ShimDir+"/post-commit", "#!/bin/sh\necho user-owned\n")
				if out, err := s.run(nested, "", s.bin, args...); err == nil {
					t.Fatalf("expected rejection of user hook: %s", out)
				}
				if err := os.Remove(filepath.Join(root, hookmgr.ShimDir, "post-commit")); err != nil {
					t.Fatal(err)
				}
			}
			s.cli(nested, args...)
			s.cli(nested, args...)
			after, err := termaproject.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if before.Project != after.Project || before.Install.Version != after.Install.Version || !before.Install.InstalledAt.Equal(after.Install.InstalledAt) {
				t.Fatalf("upgrade changed identity/history: before=%+v after=%+v", before, after)
			}
			if after.Install.HookManager != "git" {
				t.Fatalf("Git hooks not recorded: %+v", after.Install)
			}
			if got := readInstallFile(t, root, hookmgr.ClaudeSettingsPath); !bytes.Equal(originalHooks, got) {
				t.Fatalf("upgrade churned agent hooks or policy:\n%s", got)
			}
			nowActive, _ := store.Active(time.Now(), 0)
			nowManifests, err := store.Manifests()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(active, nowActive) || !reflect.DeepEqual(manifests, nowManifests) {
				t.Fatal("reinstall changed session metadata or touch timestamps")
			}
			requireAbsent(t, filepath.Join(root, ".git", "terma", "session.json"))
			requireAbsent(t, filepath.Join(root, ".git", "terma", "manifests"))

			if kind != "active_without_manifest" {
				s.write(root, "human.txt", "human work\n")
				s.git(root, "add", "human.txt")
				s.git(root, "commit", "-qm", "Human work")
				if msg := s.git(root, "log", "-1", "--format=%B"); strings.Contains(msg, "Agent-Session-Id:") {
					t.Fatalf("active session claimed human work: %s", msg)
				}
			}
			s.git(root, append([]string{"add", "--"}, files...)...)
			s.git(root, "commit", "-qm", "Agent work across git init")
			msg := s.git(root, "log", "-1", "--format=%B")
			for _, id := range ids {
				if !strings.Contains(msg, "Agent-Session-Id: "+id) {
					t.Fatalf("lost attribution for %s:\n%s", id, msg)
				}
			}
			if strings.Count(msg, "Agent-Session-Id:") != len(ids) {
				t.Fatalf("unexpected or duplicate attribution:\n%s", msg)
			}
			if kind != "active_without_manifest" {
				consumed, err := store.Manifests()
				if err != nil {
					t.Fatal(err)
				}
				if len(consumed) != len(ids) {
					t.Fatalf("empty tracking manifests lost: %+v", consumed)
				}
				for _, m := range consumed {
					if len(m.Files) != 0 {
						t.Fatalf("commit did not consume old state: %+v", m)
					}
				}
				s.cli(nested, args...)
				s.write(root, "human.txt", "more human work\n")
				s.git(root, "add", "human.txt")
				s.git(root, "commit", "-qm", "More human work")
				if msg := s.git(root, "log", "-1", "--format=%B"); strings.Contains(msg, "Agent-Session-Id:") {
					t.Fatalf("retry resurrected consumed attribution: %s", msg)
				}
			}
			s.cli(nested, "uninstall", "--yes")
			requireAbsent(t, private)
			requireAbsent(t, filepath.Join(root, ".git", "terma"))
			requireAbsent(t, termaproject.Path(root))
			if out, err := s.run(root, "", "git", "config", "--get", "core.hooksPath"); err == nil {
				t.Fatalf("Git hook setting survived: %s", out)
			}
			s.cli(root, "uninstall", "--yes")
		})
	}
}

func TestInstallE2ETransitionKeepsOtherWorkspaces(t *testing.T) {
	s := newInstallSandbox(t)
	root := s.mkdir("workspace")
	sibling := s.mkdir("sibling")
	s.install(root)
	s.install(sibling)
	s.claudeEvent(root, root, "SessionStart", "main-session", "")
	s.claudeEvent(sibling, sibling, "SessionStart", "sibling-session", "")
	s.git(root, "init", "-q")
	s.install(root)
	s.git(root, "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-qm", "initial")
	linked := filepath.Join(s.base, "linked")
	s.git(root, "worktree", "add", "-qb", "linked", linked)
	s.install(linked)
	s.claudeEvent(linked, linked, "SessionStart", "linked-session", "")
	gitDir := s.git(linked, "rev-parse", "--absolute-git-dir")
	if active, _ := session.Open(gitDir).Active(time.Now(), 0); active == nil || active.ID != "linked-session" {
		t.Fatalf("linked worktree did not get its own Git store: %+v", active)
	}
	s.cli(root, "uninstall", "--yes")
	for dir, id := range map[string]string{sibling: "sibling-session", linked: "linked-session"} {
		if status := s.cli(dir, "status"); !strings.Contains(status, id) {
			t.Fatalf("uninstall damaged %s state:\n%s", id, status)
		}
	}
	if got := s.git(linked, "config", "--get", "core.hooksPath"); got != hookmgr.ShimDir {
		t.Fatalf("uninstall damaged linked hooks: %s", got)
	}
	s.cli(sibling, "uninstall", "--yes")
	s.cli(linked, "uninstall", "--yes")
	dirs, err := filepath.Glob(filepath.Join(s.base, "config", "workspaces", "*"))
	if err != nil || len(dirs) != 0 {
		t.Fatalf("private stores left behind: %v %v", dirs, err)
	}
	requireAbsent(t, filepath.Join(gitDir, "terma"))
}
