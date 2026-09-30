package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hookrun/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The OpenCode plugin announces the session, names the files it edits, and the commit
// that follows carries the session and the tool.
func TestOpenCodeSessionIsStampedOnItsCommit(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string, args ...string) hookrun.Env {
		return hookrun.Env{Now: time.Now(), Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}

	if err := sessionStart(ctx, env(`{"session_id":"ses_abc123","cwd":"`+root+`"}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/a.go", "package src\n")
	hookruntest.WriteFile(t, root, "src/b.go", "package src\n")
	// One absolute path, one relative to the working directory the plugin reported.
	if err := fileEdit(ctx, env(`{"session_id":"ses_abc123","cwd":"`+root+`","file":"`+filepath.Join(root, "src", "a.go")+`","tool":"edit"}`)); err != nil {
		t.Fatal(err)
	}
	if err := fileEdit(ctx, env(`{"session_id":"ses_abc123","cwd":"`+root+`","file":"src/b.go"}`)); err != nil {
		t.Fatal(err)
	}

	if _, err := gitx.Git(ctx, root, "add", "src"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("agent work\n"), 0o644)
	if err := hookrun.PrepareCommitMsg(ctx, env("", msgPath, "message")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if !strings.Contains(string(data), "Agent-Session-Id: ses_abc123") || !strings.Contains(string(data), "Agent-Tool: opencode") {
		t.Fatalf("commit not stamped for OpenCode:\n%s", data)
	}

	if err := sessionEnd(ctx, env(`{"session_id":"ses_abc123","cwd":"`+root+`","reason":"deleted"}`)); err != nil {
		t.Fatal(err)
	}
	events, _, _ := sp.Pending()
	if events < 4 {
		t.Fatalf("expected start, two file events and an end in the spool, got %d", events)
	}
}

// Anything the handlers cannot use is ignored, never an error: a hook that fails would
// surface inside the agent.
func TestOpenCodeHandlersIgnoreBadInput(t *testing.T) {
	ctx := context.Background()
	for _, stdin := range []string{"", "not json", `{"cwd":"/x"}`, `{"session_id":"../../etc"}`} {
		env := hookrun.Env{Cwd: t.TempDir(), Stdin: strings.NewReader(stdin)}
		if err := sessionStart(ctx, env); err != nil {
			t.Errorf("start(%q) = %v", stdin, err)
		}
		if err := fileEdit(ctx, env); err != nil {
			t.Errorf("edit(%q) = %v", stdin, err)
		}
		if err := sessionEnd(ctx, env); err != nil {
			t.Errorf("end(%q) = %v", stdin, err)
		}
	}
}

// An OpenCode session the task tool opened names the session that opened it.
func TestOpenCodeChildSessionNamesItsParent(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string) hookrun.Env {
		return hookrun.Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	if err := sessionStart(ctx, env(`{"session_id":"ses_child","cwd":"`+root+`","parent_session_id":"ses_parent"}`)); err != nil {
		t.Fatal(err)
	}
	if err := sessionStart(ctx, env(`{"session_id":"ses_root","cwd":"`+root+`"}`)); err != nil {
		t.Fatal(err)
	}
	if err := sessionStart(ctx, env(`{"session_id":"ses_odd","cwd":"`+root+`","parent_session_id":"../etc"}`)); err != nil {
		t.Fatal(err)
	}
	events := hookruntest.Spooled(t, sp)
	if len(events) != 3 || events[0].Attrs["parent_session_id"] != "ses_parent" {
		t.Fatalf("events: %+v", events)
	}
	for _, e := range events[1:] {
		if _, ok := e.Attrs["parent_session_id"]; ok {
			t.Fatalf("%s reports a parent: %v", e.SessionID, e.Attrs)
		}
	}
}

// The active session is what claims a commit no manifest accounts for. A session the
// task tool opened for a subagent must not displace the one a person is driving, or the
// developer's next hand-written commit is stamped with the subagent.
func TestOpenCodeChildSessionNeverBecomesTheActiveOne(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string, args ...string) hookrun.Env {
		return hookrun.Env{Now: time.Now(), Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	if err := sessionStart(ctx, env(`{"session_id":"ses_person","cwd":"`+root+`"}`)); err != nil {
		t.Fatal(err)
	}
	if err := sessionStart(ctx, env(`{"session_id":"ses_child","cwd":"`+root+`","parent_session_id":"ses_person"}`)); err != nil {
		t.Fatal(err)
	}
	active, _ := hookruntest.Store(t, root).Active(time.Now(), 0)
	if active == nil || active.ID != "ses_person" {
		t.Fatalf("active session = %+v, want the person's", active)
	}
	// The child is still announced, with its parent.
	events := hookruntest.Lifecycle(hookruntest.Spooled(t, sp))
	if len(events) != 2 || events[1].SessionID != "ses_child" || events[1].Attrs[hookrun.AttrParentSession] != "ses_person" {
		t.Fatalf("events: %+v", events)
	}
}
