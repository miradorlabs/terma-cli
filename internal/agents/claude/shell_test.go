package claude

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
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// A file a session writes through Bash joins its manifest, so a commit made by hand later
// names the session. A file that was already changed and that the call left alone, or one
// another session's edit tool wrote while the call ran, does not.
func TestBashEditsJoinTheSessionsManifest(t *testing.T) {
	root := newRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	now := time.Now()
	env := func(at time.Time, stdin string, args ...string) hookrun.Env {
		return hookrun.Env{Now: at, Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	hookruntest.WriteFile(t, root, "README.md", "base\n")
	hookruntest.WriteFile(t, root, "tracked.txt", "base\n")
	if _, err := gitx.Git(ctx, root, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(ctx, root, "commit", "-qm", "base"); err != nil {
		t.Fatal(err)
	}
	if err := sessionStart(ctx, env(now, `{"session_id":"sess-bash","cwd":"`+root+`","hook_event_name":"SessionStart","source":"startup"}`)); err != nil {
		t.Fatal(err)
	}
	// The developer's own uncommitted edit, there before the call.
	hookruntest.WriteFile(t, root, "README.md", "mine\n")
	bash := `{"session_id":"sess-bash","cwd":"` + root + `","tool_name":"Bash","tool_use_id":"toolu_1","tool_input":{"command":"echo resumed >> tracked.txt && echo new > made.txt"}}`
	if err := preToolUse(ctx, env(now, bash)); err != nil {
		t.Fatal(err)
	}
	// What the command did, and another session's Write landing in the same window.
	hookruntest.WriteFile(t, root, "tracked.txt", "base\nresumed\n")
	hookruntest.WriteFile(t, root, "made.txt", "new\n")
	hookruntest.WriteFile(t, root, "theirs.go", "package theirs\n")
	other := `{"session_id":"sess-other","cwd":"` + root + `","tool_name":"Write","tool_input":{"file_path":"` + filepath.Join(root, "theirs.go") + `"}}`
	if err := postToolUse(ctx, env(now.Add(time.Second), other)); err != nil {
		t.Fatal(err)
	}
	if err := postToolUse(ctx, env(now.Add(2*time.Second), bash)); err != nil {
		t.Fatal(err)
	}

	manifests, err := session.Open(filepath.Join(root, ".git")).Manifests()
	if err != nil {
		t.Fatal(err)
	}
	var mine map[string]time.Time
	for _, m := range manifests {
		if m.SessionID == "sess-bash" {
			mine = m.Files
		}
	}
	if _, ok := mine["tracked.txt"]; !ok || len(mine) != 2 {
		t.Fatalf("the Bash call's manifest = %v, want tracked.txt and made.txt only", mine)
	}
	if _, ok := mine["made.txt"]; !ok {
		t.Fatalf("a file the Bash call created is missing: %v", mine)
	}
	var touched *spool.Event
	for _, e := range hookruntest.Spooled(t, sp) {
		if e.Name == hookrun.EventFilesTouched && e.SessionID == "sess-bash" {
			touched = &e
		}
	}
	if touched == nil || touched.Attrs[hookrun.AttrEditSource] != "shell" || touched.Attrs[hookrun.AttrToolName] != "Bash" {
		t.Fatalf("files.touched for the Bash call = %+v", touched)
	}

	// The session ends; the developer commits the Bash-written file by hand.
	if err := sessionEnd(ctx, env(now.Add(3*time.Second), `{"session_id":"sess-bash","cwd":"`+root+`","hook_event_name":"SessionEnd"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(ctx, root, "add", "tracked.txt"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("by hand\n"), 0o644)
	if err := hookrun.PrepareCommitMsg(ctx, env(now.Add(time.Hour), "", msgPath, "message")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if got := trailer.Parse(string(data), "#"); len(got) != 1 || got[0].SessionID != "sess-bash" {
		t.Fatalf("a hand commit of a Bash-written file is not stamped with its session:\n%s", data)
	}
}

// A PostToolUse whose PreToolUse never ran claims nothing: the tree's changes predate it.
func TestBashWithoutASnapshotRecordsNothing(t *testing.T) {
	root := newRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := hookrun.Env{Now: time.Now(), Cwd: root, Spool: sp, Version: "test"}
	hookruntest.WriteFile(t, root, "dirty.txt", "x\n")
	env.Stdin = strings.NewReader(`{"session_id":"sess-late","cwd":"` + root + `","tool_name":"Bash","tool_use_id":"toolu_2","tool_input":{"command":"ls"}}`)
	if err := postToolUse(ctx, env); err != nil {
		t.Fatal(err)
	}
	if manifests, _ := session.Open(filepath.Join(root, ".git")).Manifests(); len(manifests) != 0 {
		t.Fatalf("an unseen call's tree was attributed: %+v", manifests)
	}
}
