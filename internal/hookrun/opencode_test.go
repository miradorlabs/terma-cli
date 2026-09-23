package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The OpenCode plugin announces the session, names the files it edits, and the commit
// that follows carries the session and the tool.
func TestOpenCodeSessionIsStampedOnItsCommit(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string, args ...string) Env {
		return Env{Now: time.Now(), Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}

	if err := OpenCodeSessionStart(ctx, env(`{"session_id":"ses_abc123","cwd":"`+root+`"}`)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "src/a.go", "package src\n")
	writeFile(t, root, "src/b.go", "package src\n")
	// One absolute path, one relative to the working directory the plugin reported.
	if err := OpenCodeFileEdit(ctx, env(`{"session_id":"ses_abc123","cwd":"`+root+`","file":"`+filepath.Join(root, "src", "a.go")+`","tool":"edit"}`)); err != nil {
		t.Fatal(err)
	}
	if err := OpenCodeFileEdit(ctx, env(`{"session_id":"ses_abc123","cwd":"`+root+`","file":"src/b.go"}`)); err != nil {
		t.Fatal(err)
	}

	if _, err := gitx.Git(ctx, root, "add", "src"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("agent work\n"), 0o644)
	if err := PrepareCommitMsg(ctx, env("", msgPath, "message")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if !strings.Contains(string(data), "Agent-Session-Id: ses_abc123") || !strings.Contains(string(data), "Agent-Tool: opencode") {
		t.Fatalf("commit not stamped for OpenCode:\n%s", data)
	}

	if err := OpenCodeSessionEnd(ctx, env(`{"session_id":"ses_abc123","cwd":"`+root+`","reason":"deleted"}`)); err != nil {
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
		env := Env{Cwd: t.TempDir(), Stdin: strings.NewReader(stdin)}
		if err := OpenCodeSessionStart(ctx, env); err != nil {
			t.Errorf("start(%q) = %v", stdin, err)
		}
		if err := OpenCodeFileEdit(ctx, env); err != nil {
			t.Errorf("edit(%q) = %v", stdin, err)
		}
		if err := OpenCodeSessionEnd(ctx, env); err != nil {
			t.Errorf("end(%q) = %v", stdin, err)
		}
	}
}
