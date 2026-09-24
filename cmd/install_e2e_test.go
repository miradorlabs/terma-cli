//go:build unix

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// Run the built CLI, never Cobra in process. Each scenario gets private config,
// no inherited credentials/exporters/Git overrides, and a bounded subprocess.
type installSandbox struct {
	t         *testing.T
	bin, base string
	env       []string
}

func newInstallSandbox(t *testing.T) *installSandbox {
	t.Helper()
	base := t.TempDir()
	base, _ = filepath.EvalSymlinks(base)
	bin := termaBinary(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PATH=" + filepath.Dir(bin) + ":" + filepath.Dir(git) + ":/usr/bin:/bin",
		"TERMA_ENV=dev", "TERMA_LIVE=0", "TERMA_CONFIG_DIR=" + filepath.Join(base, "config"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(base, "claude-config"), "XDG_CONFIG_HOME=" + filepath.Join(base, "xdg"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"SHELL=/bin/sh",
	}
	return &installSandbox{t: t, bin: bin, base: base, env: env}
}
func (s *installSandbox) run(dir, input, bin string, args ...string) (string, error) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bin, args...)
	c.Dir = dir
	c.Env = s.env
	c.Stdin = strings.NewReader(input)
	out, err := c.CombinedOutput()
	return string(out), err
}
func (s *installSandbox) cli(dir string, args ...string) string {
	s.t.Helper()
	out, err := s.run(dir, "", s.bin, args...)
	if err != nil {
		s.t.Fatalf("terma %v: %v\n%s", args, err, out)
	}
	return out
}
func (s *installSandbox) git(dir string, args ...string) string {
	s.t.Helper()
	out, err := s.run(dir, "", "git", args...)
	if err != nil {
		s.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(out)
}
func (s *installSandbox) mkdir(parts ...string) string {
	s.t.Helper()
	p := filepath.Join(append([]string{s.base}, parts...)...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		s.t.Fatal(err)
	}
	return p
}
func (s *installSandbox) write(root, path, body string) {
	s.t.Helper()
	p := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		s.t.Fatal(err)
	}
}
func (s *installSandbox) install(dir string, extra ...string) string {
	return s.cli(dir, append([]string{"install", "--harness", "none", "--project", testProjectID, "--adapters", "claude,cursor,codex,antigravity", "--yes", "--no-doctor"}, extra...)...)
}
func readInstallFile(t *testing.T, root, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected absent %s: %v", path, err)
	}
}

func TestInstallE2ELocations(t *testing.T) {
	for _, kind := range []string{"root", "deep", "spaces_unicode", "symlink", "nested_repository", "separate_git_dir", "linked_worktree", "detached_worktree", "bare_parent_worktree", "explicit_git_environment", "submodule", "non_git", "non_git_without_git"} {
		t.Run(kind, func(t *testing.T) {
			s := newInstallSandbox(t)
			root := s.mkdir("workspace")
			isGit := !strings.HasPrefix(kind, "non_git")
			if kind == "spaces_unicode" {
				root = s.mkdir("project with spaces ' ü $dollar")
			}
			if isGit {
				if kind == "separate_git_dir" {
					s.git(root, "init", "-q", "--separate-git-dir", filepath.Join(s.base, "metadata"))
				} else {
					s.git(root, "init", "-q")
				}
			}
			switch kind {
			case "linked_worktree", "detached_worktree", "bare_parent_worktree":
				s.git(root, "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-qm", "initial")
				linked := filepath.Join(s.base, "linked")
				switch kind {
				case "detached_worktree":
					s.git(root, "worktree", "add", "--detach", linked)
				case "bare_parent_worktree":
					bare := filepath.Join(s.base, "bare")
					s.git(s.base, "clone", "--bare", root, bare)
					s.git(bare, "worktree", "add", "-b", "linked", linked)
				default:
					s.git(root, "worktree", "add", "-qb", "linked", linked)
				}
				root = linked
			case "submodule":
				source := s.mkdir("source")
				s.git(source, "init", "-q")
				s.git(source, "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-qm", "initial")
				s.git(root, "-c", "protocol.file.allow=always", "submodule", "add", source, "modules/child")
				root = filepath.Join(root, "modules/child")
			case "nested_repository":
				s.install(root)
				root = s.mkdir("workspace", "inner")
				s.git(root, "init", "-q")
			case "explicit_git_environment":
				s.env = append(s.env, "GIT_DIR="+filepath.Join(root, ".git"), "GIT_WORK_TREE="+root)
			case "non_git_without_git":
				// The binary is addressed absolutely, so this PATH intentionally has no Git.
				s.env = append(s.env, "PATH="+filepath.Dir(s.bin))
			}
			cwd := root
			if kind != "root" && isGit {
				cwd = filepath.Join(root, "one", "two", "three", "four", "five")
				if err := os.MkdirAll(cwd, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				link := filepath.Join(s.base, "alias")
				if err := os.Symlink(root, link); err != nil {
					t.Fatal(err)
				}
				cwd = filepath.Join(link, "one", "two")
			}
			s.install(cwd)
			bound, err := termaproject.Load(root)
			if err != nil || bound.Project.ID != testProjectID {
				t.Fatalf("binding: %+v %v", bound, err)
			}
			for _, path := range []string{hookmgr.ClaudeSettingsPath, hookmgr.CursorHooksPath, hookmgr.CodexHooksPath, hookmgr.AntigravityHooksPath} {
				if !bytes.Contains(readInstallFile(t, root, path), []byte("terma hook")) {
					t.Fatalf("missing hooks in %s", path)
				}
			}
			if isGit {
				if got := s.git(root, "config", "--get", "core.hooksPath"); got != hookmgr.ShimDir {
					t.Fatalf("hooksPath=%q", got)
				}
			} else {
				requireAbsent(t, filepath.Join(root, hookmgr.ShimDir))
				if bound.Install.HookManager != "" || len(bound.Install.Hooks) != 0 {
					t.Fatalf("recorded Git hooks: %+v", bound.Install)
				}
			}
			first := readInstallFile(t, root, termaproject.FileName)
			nested := filepath.Join(root, "nested", "deeper")
			if err := os.MkdirAll(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			s.install(nested)
			if !bytes.Equal(first, readInstallFile(t, root, termaproject.FileName)) {
				t.Fatal("repeat install churned binding")
			}
			requireAbsent(t, filepath.Join(nested, termaproject.FileName))
			s.cli(nested, "uninstall", "--yes")
			requireAbsent(t, termaproject.Path(root))
			if kind == "nested_repository" {
				if bound, err := termaproject.Load(filepath.Dir(root)); err != nil || bound.Project.ID != testProjectID {
					t.Fatalf("child uninstall damaged parent binding: %+v %v", bound, err)
				}
				if out := s.cli(root, "status"); !strings.Contains(out, "not installed") {
					t.Fatalf("child inherited parent binding: %s", out)
				}
			}
			for _, path := range []string{hookmgr.ClaudeSettingsPath, hookmgr.CodexHooksPath, hookmgr.AntigravityHooksPath, hookmgr.ShimDir + "/post-commit"} {
				requireAbsent(t, filepath.Join(root, path))
			}
			if bytes.Contains(readInstallFile(t, root, hookmgr.CursorHooksPath), []byte("terma hook")) {
				t.Fatal("Cursor commands survived")
			}
			if out := s.cli(root, "uninstall", "--yes"); !strings.Contains(out, "Nothing") {
				t.Fatalf("repeat uninstall: %s", out)
			}
		})
	}
}

func TestInstallE2EPreservesUserFiles(t *testing.T) {
	for _, manager := range []string{"git", "husky", "lefthook", "pre-commit", "non_git"} {
		t.Run(manager, func(t *testing.T) {
			s := newInstallSandbox(t)
			root := s.mkdir("workspace")
			if manager != "non_git" {
				s.git(root, "init", "-q")
			}
			fixtures := map[string]string{
				".claude/settings.json": `{"permissions":{"allow":["Read"]},"env":{"MY_FLAG":"keep","OTEL_LOGS_EXPORTER":"console"},"hooks":{"PostToolUse":[{"matcher":"Write","hooks":[{"type":"command","command":"./my-audit.sh"}]}]}}`,
				".cursor/hooks.json":    `{"version":42,"custom":"keep","hooks":{"afterFileEdit":[{"command":"./my-audit.sh"}]}}`,
				".codex/hooks.json":     `{"custom":"keep","hooks":{"PostToolUse":[{"matcher":".*","hooks":[{"type":"command","command":"./my-audit.sh"}]}]}}`,
				".agents/hooks.json":    `{"team":{"Stop":[{"type":"command","command":"./my-audit.sh"}]}}`,
				".terma/notes.txt":      "user notes\n",
			}
			switch manager {
			case "git":
				fixtures[".git/hooks/post-commit"] = "#!/bin/sh\necho team-hook\n"
			case "husky":
				fixtures[".husky/post-commit"] = "#!/bin/sh\necho team-hook\necho 'terma hook post-commit'\n"
			case "lefthook":
				fixtures["lefthook.yml"] = "post-commit:\n  commands:\n    team:\n      run: echo team-hook\n"
			case "pre-commit":
				fixtures[".pre-commit-config.yaml"] = "repos:\n  - repo: local\n    hooks:\n      - id: team\n        name: team\n        entry: echo team-hook\n        language: system\n"
			}
			for path, body := range fixtures {
				s.write(root, path, body)
			}
			s.install(root)
			s.install(root)
			s.cli(root, "uninstall", "--yes")
			for path, want := range fixtures {
				got := readInstallFile(t, root, path)
				if strings.HasSuffix(path, ".json") {
					var a, b any
					if err := json.Unmarshal([]byte(want), &a); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(got, &b); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(a, b) {
						t.Fatalf("user JSON changed %s:\n%s", path, got)
					}
				} else if !bytes.Equal(got, []byte(want)) {
					t.Fatalf("user file changed %s:\n%s", path, got)
				}
			}
		})
	}
}

func TestInstallE2ERejectsBrokenInputsWithoutWrites(t *testing.T) {
	for _, kind := range []string{"bare", "broken_git_file", "broken_binding", "broken_hooks", "null_hooks", "null_document", "shim_collision", "newline_path"} {
		t.Run(kind, func(t *testing.T) {
			s := newInstallSandbox(t)
			root := s.mkdir("workspace")
			switch kind {
			case "bare":
				s.git(root, "init", "--bare", "-q")
			case "broken_git_file":
				s.write(root, ".git", "gitdir: /definitely/missing\n")
			default:
				s.git(root, "init", "-q")
			}
			if kind == "newline_path" {
				root = s.mkdir("newline\nworkspace")
				s.git(root, "init", "-q")
			}
			path, body := "", ""
			switch kind {
			case "broken_binding":
				path = termaproject.FileName
				body = `{"project":`
			case "broken_hooks":
				path = hookmgr.ClaudeSettingsPath
				body = `{"hooks":`
			case "null_hooks":
				path = hookmgr.ClaudeSettingsPath
				body = `{"hooks":null}`
			case "null_document":
				path = hookmgr.ClaudeSettingsPath
				body = `null`
			case "shim_collision":
				path = hookmgr.ShimDir + "/post-commit"
				body = "#!/bin/sh\necho owned-by-user\n"
			}
			if path != "" {
				s.write(root, path, body)
			}
			out, err := s.run(root, "", s.bin, "install", "--harness", "none", "--project", testProjectID, "--yes", "--no-doctor")
			if err == nil {
				t.Fatalf("expected rejection: %s", out)
			}
			if path != "" && string(readInstallFile(t, root, path)) != body {
				t.Fatal("changed rejected input")
			}
			if kind != "broken_binding" {
				requireAbsent(t, termaproject.Path(root))
			}
			requireAbsent(t, filepath.Join(root, hookmgr.CursorHooksPath))
		})
	}
}

func TestInstallE2EWorktreesStayIndependent(t *testing.T) {
	s := newInstallSandbox(t)
	main := s.mkdir("main")
	s.git(main, "init", "-q")
	s.git(main, "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-qm", "initial")
	linked := filepath.Join(s.base, "linked")
	s.git(main, "worktree", "add", "-qb", "linked", linked)
	s.install(linked)
	if out, err := s.run(main, "", "git", "config", "--get", "core.hooksPath"); err == nil {
		t.Fatalf("linked install changed main hooksPath: %s", out)
	}
	s.install(main)
	s.cli(linked, "uninstall", "--yes")
	if got := s.git(main, "config", "--get", "core.hooksPath"); got != hookmgr.ShimDir {
		t.Fatalf("linked uninstall changed main hooksPath: %s", got)
	}
	if out, err := s.run(linked, "", "git", "config", "--get", "core.hooksPath"); err == nil {
		t.Fatalf("linked hooksPath remains: %s", out)
	}
	s.cli(main, "uninstall", "--yes")
}

func TestInstallE2ERestoresAndChainsHooksPath(t *testing.T) {
	for _, kind := range []string{"local", "global", "tilde_path", "edited_after_install", "explicit_empty"} {
		t.Run(kind, func(t *testing.T) {
			s := newInstallSandbox(t)
			root := s.mkdir("workspace")
			s.git(root, "init", "-q")
			hookdir := s.mkdir("team hooks")
			s.write(hookdir, "prepare-commit-msg", "#!/bin/sh\nprintf 'team-hook\\n' >> team-hook-ran\n")
			initial := hookdir
			if kind == "tilde_path" {
				s.env = append(s.env, "HOME="+s.base)
				initial = "~/team hooks"
			}
			if kind == "global" {
				global := filepath.Join(s.base, "gitconfig")
				s.env = append(s.env, "GIT_CONFIG_GLOBAL="+global)
				s.git(root, "config", "--global", "core.hooksPath", hookdir)
			} else {
				if kind == "explicit_empty" {
					initial = ""
				}
				s.git(root, "config", "--local", "core.hooksPath", initial)
			}
			s.install(root)
			if kind != "explicit_empty" {
				s.git(root, "commit", "--allow-empty", "-qm", "check chaining")
				if got := string(readInstallFile(t, root, "team-hook-ran")); got != "team-hook\n" {
					t.Fatalf("previous hook did not run exactly once: %q", got)
				}
			}
			if kind == "edited_after_install" {
				initial = "new-user-hooks"
				s.git(root, "config", "--worktree", "core.hooksPath", initial)
			}
			s.cli(root, "uninstall", "--yes")
			if got := s.git(root, "config", "--get", "core.hooksPath"); got != initial {
				t.Fatalf("restored %q, want %q", got, initial)
			}
			if kind == "global" {
				if out, err := s.run(root, "", "git", "config", "--local", "--get", "core.hooksPath"); err == nil {
					t.Fatalf("pinned inherited config locally: %s", out)
				}
			}
		})
	}
}

func TestInstallE2EMixedHooksAndLookalikes(t *testing.T) {
	s := newInstallSandbox(t)
	root := s.mkdir("workspace")
	s.git(root, "init", "-q")
	body := `{"hooks":{"PostToolUse":[{"matcher":"Write","hooks":[{"type":"command","command":"terma hook post-tool-use"},{"type":"command","command":"./audit.sh"},{"type":"command","command":"echo 'terma hook post-tool-use'"}]}]}}`
	s.write(root, hookmgr.ClaudeSettingsPath, body)
	s.write(root, hookmgr.CursorHooksPath, `{"version":42}`)
	s.install(root)
	got := string(readInstallFile(t, root, hookmgr.ClaudeSettingsPath))
	if !strings.Contains(got, "./audit.sh") || !strings.Contains(got, "echo 'terma hook post-tool-use'") {
		t.Fatalf("install erased user handlers: %s", got)
	}
	s.cli(root, "uninstall", "--yes")
	got = string(readInstallFile(t, root, hookmgr.ClaudeSettingsPath))
	if !strings.Contains(got, "./audit.sh") || !strings.Contains(got, "echo 'terma hook post-tool-use'") {
		t.Fatalf("uninstall erased user handlers: %s", got)
	}
	var doc struct {
		Hooks map[string][]struct{ Hooks []struct{ Command string } }
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Hooks["PostToolUse"]) != 1 || len(doc.Hooks["PostToolUse"][0].Hooks) != 2 {
		t.Fatalf("owned command survived or user group changed: %s", got)
	}
	if !strings.Contains(string(readInstallFile(t, root, hookmgr.CursorHooksPath)), "42") {
		t.Fatal("user schema version removed")
	}
}

func TestInstallE2EModifiedShimSurvivesUninstall(t *testing.T) {
	s := newInstallSandbox(t)
	root := s.mkdir("workspace")
	s.git(root, "init", "-q")
	s.install(root)
	path := hookmgr.ShimDir + "/post-commit"
	body := string(readInstallFile(t, root, path)) + "# user addition\n"
	s.write(root, path, body)
	out := s.cli(root, "uninstall", "--yes")
	if !strings.Contains(out, "Left modified") || string(readInstallFile(t, root, path)) != body {
		t.Fatalf("modified shim lost or not reported: %s", out)
	}
}

func TestInstallE2ENonGitHooksActuallyRun(t *testing.T) {
	s := newInstallSandbox(t)
	root := s.mkdir("workspace")
	s.install(root)
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct{ Hooks []struct{ Command string } }
	}
	if err := json.Unmarshal(readInstallFile(t, root, hookmgr.ClaudeSettingsPath), &doc); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"SessionStart", "PostToolUse"} {
		payload, _ := json.Marshal(map[string]any{"session_id": "e2e-session", "cwd": nested, "hook_event_name": event, "tool_name": "Write", "tool_input": map[string]string{"file_path": filepath.Join(nested, "edited.txt")}})
		command := doc.Hooks[event][0].Hooks[0].Command
		if out, err := s.run(nested, string(payload), "/bin/sh", "-c", command); err != nil {
			t.Fatalf("installed hook: %v %s", err, out)
		}
	}
	status := s.cli(nested, "status")
	if !strings.Contains(status, "e2e-session") || !strings.Contains(status, "1 agent-edited file") || !strings.Contains(status, "Git hooks skipped") {
		t.Fatalf("non-Git hooks inactive: %s", status)
	}
	// No credential means doctor cannot contact a backend. Git checks must still skip.
	out, _ := s.run(nested, "", s.bin, "doctor", "--skip-commit")
	if !strings.Contains(out, "not a Git repository") || strings.Contains(out, "needs an installed repository") {
		t.Fatalf("non-Git diagnostic: %s", out)
	}
	s.cli(nested, "uninstall", "--yes")
	matches, err := filepath.Glob(filepath.Join(s.base, "config", "workspaces", "*", "terma", "session.json"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("session state survived: %v %v", matches, err)
	}
}

func TestInstallE2EDryRunAndNoHooks(t *testing.T) {
	for _, nonGit := range []bool{false, true} {
		t.Run(map[bool]string{false: "git", true: "non_git"}[nonGit], func(t *testing.T) {
			s := newInstallSandbox(t)
			root := s.mkdir("workspace")
			if !nonGit {
				s.git(root, "init", "-q")
			}
			s.install(root, "--dry-run")
			for _, path := range []string{termaproject.Dir, ".claude", ".cursor", ".codex", ".agents"} {
				requireAbsent(t, filepath.Join(root, path))
			}
			s.install(root, "--no-hooks")
			for _, path := range []string{hookmgr.ShimDir, ".claude", ".cursor", ".codex", ".agents"} {
				requireAbsent(t, filepath.Join(root, path))
			}
			s.cli(root, "uninstall", "--yes")
		})
	}
}

func TestInstallE2ESymlinkedConfigIsNotModified(t *testing.T) {
	for _, kind := range []string{"agent_file", "agent_directory", "binding_directory"} {
		t.Run(kind, func(t *testing.T) {
			s := newInstallSandbox(t)
			root := s.mkdir("workspace")
			s.git(root, "init", "-q")
			external := s.mkdir("external")
			body := `{"hooks":{},"user":"keep"}`
			target := filepath.Join(external, "settings.json")
			s.write(external, "settings.json", body)
			var link string
			switch kind {
			case "agent_file":
				if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(root, hookmgr.ClaudeSettingsPath)
			case "agent_directory":
				link = filepath.Join(root, ".claude")
				target = external
			case "binding_directory":
				link = filepath.Join(root, termaproject.Dir)
				target = external
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			for _, command := range []string{"install", "uninstall"} {
				args := []string{command, "--yes"}
				if command == "install" {
					args = append(args, "--harness", "none", "--project", testProjectID, "--no-doctor")
				}
				out, err := s.run(root, "", s.bin, args...)
				if err == nil {
					t.Fatalf("expected symlink refusal: %s", out)
				}
				if string(readInstallFile(t, external, "settings.json")) != body {
					t.Fatal("external config was modified")
				}
				if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("symlink was replaced")
				}
			}
		})
	}
}

func TestInstallE2EUninstallOwnership(t *testing.T) {
	for _, kind := range []string{"journal_restores", "journal_missing", "user_changed_after_install"} {
		t.Run(kind, func(t *testing.T) {
			s := newInstallSandbox(t)
			root := s.mkdir("workspace")
			s.git(root, "init", "-q")
			s.write(root, hookmgr.ClaudeSettingsPath, `{"env":{"USER_FLAG":"keep","OTEL_LOG_USER_PROMPTS":"0"}}`)
			s.install(root, "--exclude-prompts=false")
			switch kind {
			case "journal_missing":
				s.env = append(s.env, "TERMA_CONFIG_DIR="+filepath.Join(s.base, "other-machine"))
			case "user_changed_after_install":
				var doc map[string]any
				if err := json.Unmarshal(readInstallFile(t, root, hookmgr.ClaudeSettingsPath), &doc); err != nil {
					t.Fatal(err)
				}
				doc["env"].(map[string]any)["OTEL_LOG_USER_PROMPTS"] = "user-choice"
				data, _ := json.Marshal(doc)
				s.write(root, hookmgr.ClaudeSettingsPath, string(data))
			}
			out := s.cli(root, "uninstall", "--yes")
			var doc struct {
				Env   map[string]string
				Hooks map[string]any
			}
			if err := json.Unmarshal(readInstallFile(t, root, hookmgr.ClaudeSettingsPath), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Env["USER_FLAG"] != "keep" || len(doc.Hooks) != 0 {
				t.Fatalf("uninstall damaged user settings or left hooks: %+v", doc)
			}
			// Without this machine's journal the policy terma wrote ("1") is still terma's
			// to remove — a colleague's committed policy is removable from any clone.
			want := map[string]string{"journal_restores": "0", "journal_missing": "", "user_changed_after_install": "user-choice"}[kind]
			if doc.Env["OTEL_LOG_USER_PROMPTS"] != want {
				t.Fatalf("policy %q, want %q", doc.Env["OTEL_LOG_USER_PROMPTS"], want)
			}
			if kind == "journal_missing" && !strings.Contains(out, "Uninstalled") {
				t.Fatalf("uninstall did not finish: %s", out)
			}
		})
	}
}

func TestInstallE2EAntigravityMixedHandlers(t *testing.T) {
	s := newInstallSandbox(t)
	root := s.mkdir("workspace")
	s.git(root, "init", "-q")
	s.write(root, hookmgr.AntigravityHooksPath, `{"terma":{"enabled":false,"custom":"keep","Stop":[{"type":"command","command":"terma hook antigravity-stop"},{"type":"command","command":"./audit.sh"}]}}`)
	s.install(root)
	s.cli(root, "uninstall", "--yes")
	got := string(readInstallFile(t, root, hookmgr.AntigravityHooksPath))
	if !strings.Contains(got, "./audit.sh") || !strings.Contains(got, "keep") || !strings.Contains(got, `"enabled": false`) || strings.Contains(got, "terma hook") {
		t.Fatalf("user handlers lost: %s", got)
	}
}

func TestInstallE2EMainInstalledBeforeWorktree(t *testing.T) {
	s := newInstallSandbox(t)
	main := s.mkdir("main")
	s.git(main, "init", "-q")
	s.install(main)
	s.git(main, "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-qm", "initial")
	linked := filepath.Join(s.base, "linked")
	s.git(main, "worktree", "add", "-qb", "linked", linked)
	s.install(linked)
	s.cli(main, "uninstall", "--yes")
	if got := s.git(linked, "config", "--get", "core.hooksPath"); got != hookmgr.ShimDir {
		t.Fatalf("main uninstall broke linked: %s", got)
	}
	if out, err := s.run(main, "", "git", "config", "--get", "core.hooksPath"); err == nil {
		t.Fatalf("main config remains: %s", out)
	}
	s.cli(linked, "uninstall", "--yes")
}

func TestInstallE2EHookManagerNameCollisions(t *testing.T) {
	for _, manager := range []string{"lefthook", "pre-commit"} {
		t.Run(manager, func(t *testing.T) {
			s := newInstallSandbox(t)
			root := s.mkdir("workspace")
			s.git(root, "init", "-q")
			path, body := "lefthook.yml", "post-commit:\n  commands:\n    terma:\n      run: ./user-command.sh\n"
			if manager == "pre-commit" {
				path = ".pre-commit-config.yaml"
				body = "repos:\n  - repo: local\n    hooks:\n      - id: terma-post-commit\n        name: user\n        entry: ./user-command.sh\n        language: system\n"
			}
			s.write(root, path, body)
			out, err := s.run(root, "", s.bin, "install", "--harness", "none", "--project", testProjectID, "--yes", "--no-doctor")
			if err == nil {
				t.Fatalf("overwrote conflicting name: %s", out)
			}
			s.cli(root, "uninstall", "--yes")
			if string(readInstallFile(t, root, path)) != body {
				t.Fatal("user hook removed based on name")
			}
		})
	}
}

func TestInstallE2EPreservesFileMode(t *testing.T) {
	s := newInstallSandbox(t)
	root := s.mkdir("workspace")
	s.git(root, "init", "-q")
	s.write(root, hookmgr.ClaudeSettingsPath, `{"user":"keep"}`)
	path := filepath.Join(root, hookmgr.ClaudeSettingsPath)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	s.install(root)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("install relaxed permissions: %v %v", info, err)
	}
	s.cli(root, "uninstall", "--yes")
	info, err = os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("uninstall relaxed permissions: %v %v", info, err)
	}
}

func TestInstallE2EUpgradesLegacyHooksPath(t *testing.T) {
	for _, previous := range []string{"", "team-hooks"} {
		t.Run("previous="+previous, func(t *testing.T) {
			s := newInstallSandbox(t)
			root := s.mkdir("workspace")
			s.git(root, "init", "-q")
			// Reproduce the on-disk shape from the released installer, rather
			// than using the current installer to create the initial state.
			s.git(root, "config", "--local", "core.hooksPath", hookmgr.ShimDir)
			record, err := json.Marshal(map[string]string{
				"previous_hooks_path": previous,
				"recorded_at":         "2026-01-01T00:00:00Z",
			})
			if err != nil {
				t.Fatal(err)
			}
			s.write(root, ".git/terma/install.json", string(record))
			if previous != "" {
				s.write(root, previous+"/prepare-commit-msg", "#!/bin/sh\nprintf 'legacy-hook\\n' >> legacy-hook-ran\n")
			}
			s.install(root)
			s.install(root)
			if got := s.git(root, "config", "--worktree", "core.hooksPath"); got != hookmgr.ShimDir {
				t.Fatalf("worktree hooks = %q", got)
			}
			if previous != "" {
				s.git(root, "commit", "--allow-empty", "-qm", "legacy chaining")
				if got := string(readInstallFile(t, root, "legacy-hook-ran")); got != "legacy-hook\n" {
					t.Fatalf("legacy hook output = %q", got)
				}
			}
			s.cli(root, "uninstall", "--yes")
			for _, scope := range []string{"--local", "--worktree"} {
				got, err := s.run(root, "", "git", "config", scope, "--get", "core.hooksPath")
				if scope == "--local" && previous != "" {
					if err != nil || strings.TrimSpace(got) != previous {
						t.Fatalf("restored local = %q, %v", got, err)
					}
				} else if err == nil {
					t.Fatalf("stale %s hooksPath = %q", scope, got)
				}
			}
		})
	}
}

func TestInstallE2ELegacyLinkedMigrationDoesNotChangeSharedHooks(t *testing.T) {
	for _, journalOwner := range []string{"main", "linked"} {
		t.Run(journalOwner, func(t *testing.T) {
			s := newInstallSandbox(t)
			main := s.mkdir("main")
			s.git(main, "init", "-q")
			s.git(main, "commit", "--allow-empty", "-qm", "initial")
			linked := filepath.Join(s.base, "linked")
			s.git(main, "worktree", "add", "-qb", "linked", linked)
			gitDir := s.git(linked, "rev-parse", "--absolute-git-dir")
			s.git(main, "config", "--local", "core.hooksPath", hookmgr.ShimDir)
			record := `{"previous_hooks_path":"team-hooks","recorded_at":"2026-01-01T00:00:00Z"}`
			journalDir := gitDir
			if journalOwner == "main" {
				journalDir = filepath.Join(main, ".git")
			}
			s.write(journalDir, "terma/install.json", record)
			out, err := s.run(linked, "", s.bin, "install", "--harness", "none", "--project", testProjectID, "--yes", "--no-doctor")
			if err == nil || !strings.Contains(out, "main worktree") {
				t.Fatalf("expected migration guidance, got %v: %s", err, out)
			}
			if got := s.git(main, "config", "--local", "core.hooksPath"); got != hookmgr.ShimDir {
				t.Fatalf("changed shared hooksPath to %q", got)
			}
			if got := string(readInstallFile(t, journalDir, "terma/install.json")); got != record {
				t.Fatalf("overwrote legacy journal: %s", got)
			}

			if journalOwner == "main" {
				if _, err := os.Stat(filepath.Join(gitDir, "terma", "install.json")); !os.IsNotExist(err) {
					t.Fatalf("refused install created a linked journal: %v", err)
				}
			}
		})
	}
}
