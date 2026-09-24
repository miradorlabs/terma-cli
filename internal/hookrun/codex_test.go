package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/desktoprelay"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func TestCodexSessionStartRegistersDesktopConversation(t *testing.T) {
	root := initRepo(t)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	const id = "01a0d0ff-0000-7000-8000-000000000003"
	if err := project.Save(root, &project.File{Project: project.Project{ID: "project-a"}}); err != nil {
		t.Fatal(err)
	}
	env := Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(`{"session_id":"` + id + `","cwd":"` + root + `"}`)}
	if err := CodexSessionStart(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if got := desktoprelay.ProjectFor(id, time.Now()); got != "project-a" {
		t.Fatalf("registered project = %q", got)
	}
}

// Codex names no edited file of its own: an edit is a tool call carrying an apply_patch
// envelope, and the paths are inside it. This is the whole reason the project hooks are
// worth having over `notify`, so it is what the end-to-end test asserts.
func TestCodexSessionStampsItsCommitFromApplyPatch(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string) Env {
		return Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	const id = "01a08bd5-0487-74b1-9d82-45e619c574fa"

	if err := CodexSessionStart(ctx, env(`{"session_id":"`+id+`","hook_event_name":"SessionStart","cwd":"`+root+`","model":"gpt-6","source":"startup","permission_mode":"default","transcript_path":null}`)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "src/a.go", "package src\n")
	writeFile(t, root, "src/b.go", "package src\n")

	patch := strings.Join([]string{
		"apply_patch <<'PATCH'",
		"*** Begin Patch",
		"*** Update File: src/a.go",
		"@@",
		"-old",
		"+new",
		"*** Add File: src/b.go",
		"+package src",
		"*** End Patch",
		"PATCH",
	}, "\\n")
	if err := CodexPostToolUse(ctx, env(`{"session_id":"`+id+`","hook_event_name":"PostToolUse","cwd":"`+root+`","model":"gpt-6","permission_mode":"default","tool_name":"apply_patch","tool_use_id":"call_1","turn_id":"turn_1","transcript_path":null,"tool_response":"ok","tool_input":{"command":"`+patch+`"}}`)); err != nil {
		t.Fatal(err)
	}
	touched := eventsNamed(spooledQuota(t, sp), EventFilesTouched)
	if len(touched) != 1 || touched[0].Attrs[attrToolCallID] != "call_1" {
		t.Fatalf("file touch must carry Codex's tool call id: %+v", touched)
	}

	if _, err := gitx.Git(ctx, root, "add", "src"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("agent work\n"), 0o644)
	commitEnv := Env{Now: time.Now(), Cwd: root, Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp, Version: "test"}
	if err := PrepareCommitMsg(ctx, commitEnv); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if !strings.Contains(string(data), "Agent-Session-Id: "+id) || !strings.Contains(string(data), "Agent-Tool: codex") {
		t.Fatalf("commit not stamped for Codex:\n%s", data)
	}

	if err := CodexSessionEnd(ctx, env(`{"session_id":"`+id+`","hook_event_name":"SessionEnd","cwd":"`+root+`","reason":"closed","transcript_path":null}`)); err != nil {
		t.Fatal(err)
	}
}

// A shell call that changed nothing must leave no manifest behind: PostToolUse fires on
// every tool call a session makes, and most of them are not edits.
func TestCodexPostToolUseIgnoresCallsWithoutAPatch(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := Env{Now: time.Now(), Cwd: root, Spool: sp, Version: "test",
		Stdin: strings.NewReader(`{"session_id":"01a08bd5-0487-74b1-9d82-45e619c574fa","hook_event_name":"PostToolUse","cwd":"` + root + `","model":"gpt-6","permission_mode":"default","tool_name":"shell","tool_use_id":"c1","turn_id":"t1","transcript_path":null,"tool_response":"","tool_input":{"command":"go test ./..."}}`)}
	if err := CodexPostToolUse(ctx, env); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("manual work\n"), 0o644)
	commitEnv := Env{Now: time.Now(), Cwd: root, Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp, Version: "test"}
	if err := PrepareCommitMsg(ctx, commitEnv); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(msgPath); strings.Contains(string(data), "Agent-Session-Id") {
		t.Fatalf("a shell command that edited nothing claimed the commit:\n%s", data)
	}
}

func TestApplyPatchPaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    []string
	}{
		{"none", "go build ./...", nil},
		{"add", "*** Begin Patch\n*** Add File: a/b.go\n*** End Patch", []string{"a/b.go"}},
		{"update and delete", "*** Update File: x.go\n*** Delete File: y.go", []string{"x.go", "y.go"}},
		// A rename touches both paths, and the commit will carry both.
		{"rename", "*** Update File: old.go\n*** Move to: new.go", []string{"old.go", "new.go"}},
		{"indented heredoc", "  *** Add File: spaced.go  ", []string{"spaced.go"}},
		{"header with no path", "*** Add File:", nil},
		{"paths with spaces", "*** Add File: dir with space/f.go", []string{"dir with space/f.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := applyPatchPaths(tc.command)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// Every handler must survive input it cannot understand: a hook that fails is a hook
// the developer removes.
func TestCodexHooksNeverFailOnBadInput(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{"", "{", `{"session_id":""}`, `{"session_id":"../../etc/passwd"}`} {
		for name, fn := range map[string]func(context.Context, Env) error{
			"session-start": CodexSessionStart,
			"session-end":   CodexSessionEnd,
			"post-tool-use": CodexPostToolUse,
		} {
			env := Env{Now: time.Now(), Cwd: t.TempDir(), Stdin: strings.NewReader(bad), Version: "test"}
			if err := fn(ctx, env); err != nil {
				t.Fatalf("%s(%q) = %v, want nil", name, bad, err)
			}
		}
	}
}
