package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// A file a Codex exec_command call writes joins the thread's manifest, so committing it by
// hand later names the thread; apply_patch keeps naming its own files.
func TestCodexShellEditsJoinTheManifest(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	const id = "01a0fc82-0487-74b1-9d82-45e619c574fa"
	env := func(stdin string, args ...string) hookrun.Env {
		return hookrun.Env{Now: time.Now(), Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	call := `{"session_id":"` + id + `","cwd":"` + root + `","tool_name":"exec_command","tool_use_id":"call_9","turn_id":"t1","tool_input":{"cmd":"sed -i '' s/a/b/ notes.txt"}}`
	if err := preToolUse(ctx, env(call)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "notes.txt", "b\n")
	if err := postToolUse(ctx, env(call)); err != nil {
		t.Fatal(err)
	}
	var touched []spool.Event
	for _, e := range hookruntest.Spooled(t, sp) {
		if e.Name == hookrun.EventFilesTouched {
			touched = append(touched, e)
		}
	}
	if len(touched) != 1 || touched[0].Attrs["files"] != "notes.txt" || touched[0].Attrs[hookrun.AttrEditSource] != "shell" ||
		touched[0].Attrs[hookrun.AttrToolCallID] != "call_9" {
		t.Fatalf("files.touched = %+v", touched)
	}
	if _, err := gitx.Git(ctx, root, "add", "notes.txt"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("by hand\n"), 0o644)
	if err := hookrun.PrepareCommitMsg(ctx, env("", msgPath, "message")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if got := trailer.Parse(string(data), "#"); len(got) != 1 || got[0].SessionID != id || got[0].Tool != "codex" {
		t.Fatalf("a hand commit of a shell-written file is not stamped with its thread:\n%s", data)
	}
}

func TestIsCodexShellCall(t *testing.T) {
	for _, tc := range []struct {
		tool, input string
		want        bool
	}{
		{"shell", `{"command":"go test ./..."}`, true},
		{"shell", `{"command":["bash","-lc","make"]}`, true},
		{"exec_command", `{"cmd":"ls"}`, true},
		{"apply_patch", `{"command":"*** Begin Patch\n*** Add File: a.go\n*** End Patch"}`, false},
		{"shell", `{"command":"cat <<'EOF' | apply_patch\n*** Update File: x.go\nEOF"}`, false},
		{"web_search", `{"query":"go"}`, false},
		{"shell", `{"command":""}`, false},
	} {
		in := &codexHookInput{ToolName: tc.tool, ToolInput: []byte(tc.input)}
		if got := isCodexShellCall(in); got != tc.want {
			t.Errorf("isCodexShellCall(%s %s) = %v, want %v", tc.tool, tc.input, got, tc.want)
		}
	}
}
