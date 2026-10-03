package omp

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
)

// The agent's hooks, as its events name them.
var (
	sessionStart = Agent{}.Events()["omp-session-start"]
	sessionEnd   = Agent{}.Events()["omp-session-end"]
	fileEdit     = Agent{}.Events()["omp-file-edit"]
)

// The omp hook announces the session, names the files it edits, and the commit that
// follows carries the session and the tool.
func TestOmpSessionIsStampedOnItsCommit(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string, args ...string) hookrun.Env {
		return hookrun.Env{Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}

	if err := sessionStart(ctx, env(`{"session_id":"5b8c2f1e-9a1b-4c2d-8e3f-0a1b2c3d4e5f","cwd":"`+root+`"}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/a.go", "package src\n")
	hookruntest.WriteFile(t, root, "src/b.go", "package src\n")
	// One absolute path, one relative to the working directory the hook reported.
	if err := fileEdit(ctx, env(`{"session_id":"5b8c2f1e-9a1b-4c2d-8e3f-0a1b2c3d4e5f","cwd":"`+root+`","file":"`+filepath.Join(root, "src", "a.go")+`","tool":"write"}`)); err != nil {
		t.Fatal(err)
	}
	if err := fileEdit(ctx, env(`{"session_id":"5b8c2f1e-9a1b-4c2d-8e3f-0a1b2c3d4e5f","file":"src/b.go","tool":"edit"}`)); err != nil {
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
	if !strings.Contains(string(data), "Agent-Session-Id: 5b8c2f1e-9a1b-4c2d-8e3f-0a1b2c3d4e5f") || !strings.Contains(string(data), "Agent-Tool: omp") {
		t.Fatalf("commit not stamped for omp:\n%s", data)
	}

	if err := sessionEnd(ctx, env(`{"session_id":"5b8c2f1e-9a1b-4c2d-8e3f-0a1b2c3d4e5f","cwd":"`+root+`"}`)); err != nil {
		t.Fatal(err)
	}
	events, _, _ := sp.Pending()
	if events < 4 {
		t.Fatalf("expected start, two file events and an end in the spool, got %d", events)
	}
}

// Anything the handlers cannot use is ignored, never an error: a hook that fails would
// surface inside the agent.
func TestOmpHandlersIgnoreBadInput(t *testing.T) {
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
