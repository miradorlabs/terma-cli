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

// Cursor's user-level hooks run from ~/.cursor, so the repository has to come from the
// workspace roots in the payload — and afterFileEdit carries no session_id, so the
// conversation id is what ties the edits to the session. Both are exercised here.
func TestCursorConversationIsStampedOnItsCommit(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	elsewhere := t.TempDir()
	env := func(stdin string, args ...string) Env {
		return Env{Now: time.Now(), Cwd: elsewhere, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	common := `"conversation_id":"conv_7f3","hook_event_name":"%s","model":"claude-opus-5","workspace_roots":["` + root + `"],"user_email":"dev@example.com"`

	if err := CursorSessionStart(ctx, env(`{`+strings.ReplaceAll(common, "%s", "sessionStart")+`,"session_id":"sess_ignored","composer_mode":"agent"}`)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "src/a.go", "package src\n")
	writeFile(t, root, "src/b.go", "package src\n")
	if err := CursorFileEdit(ctx, env(`{`+strings.ReplaceAll(common, "%s", "afterFileEdit")+`,"file_path":"`+filepath.Join(root, "src", "a.go")+`","edits":[{"old_string":"","new_string":"x"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := CursorFileEdit(ctx, env(`{`+strings.ReplaceAll(common, "%s", "afterFileEdit")+`,"file_path":"src/b.go"}`)); err != nil {
		t.Fatal(err)
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
	if !strings.Contains(string(data), "Agent-Session-Id: conv_7f3") || !strings.Contains(string(data), "Agent-Tool: cursor") {
		t.Fatalf("commit not stamped for Cursor:\n%s", data)
	}
	if strings.Contains(string(data), "sess_ignored") {
		t.Fatalf("the session was keyed on session_id, which file edits never carry:\n%s", data)
	}

	if err := CursorSessionEnd(ctx, env(`{`+strings.ReplaceAll(common, "%s", "sessionEnd")+`,"reason":"completed","duration_ms":1200}`)); err != nil {
		t.Fatal(err)
	}
	if events, _, _ := sp.Pending(); events < 4 {
		t.Fatalf("expected start, two edits and an end in the spool, got %d", events)
	}
}

func TestCursorHandlersIgnoreBadInput(t *testing.T) {
	ctx := context.Background()
	for _, stdin := range []string{"", "not json", `{"hook_event_name":"sessionStart"}`, `{"conversation_id":"../../etc"}`} {
		env := Env{Cwd: t.TempDir(), Stdin: strings.NewReader(stdin)}
		if err := CursorSessionStart(ctx, env); err != nil {
			t.Errorf("start(%q) = %v", stdin, err)
		}
		if err := CursorFileEdit(ctx, env); err != nil {
			t.Errorf("edit(%q) = %v", stdin, err)
		}
		if err := CursorSessionEnd(ctx, env); err != nil {
			t.Errorf("end(%q) = %v", stdin, err)
		}
		if err := CursorPostToolUse(ctx, env); err != nil {
			t.Errorf("tool(%q) = %v", stdin, err)
		}
		if err := CursorPostToolUseFailure(ctx, env); err != nil {
			t.Errorf("tool failure(%q) = %v", stdin, err)
		}
	}
}
