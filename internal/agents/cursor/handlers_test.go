package cursor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The repository comes from the payload's workspace roots, and the conversation id ties
// afterFileEdit (which has no session_id) to the session.
func TestCursorConversationIsStampedOnItsCommit(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	elsewhere := t.TempDir()
	env := func(stdin string, args ...string) hookrun.Env {
		return hookrun.Env{Now: time.Now(), Cwd: elsewhere, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	common := `"conversation_id":"conv_7f3","hook_event_name":"%s","model":"claude-opus-5","workspace_roots":["` + root + `"],"user_email":"dev@example.com"`

	if err := sessionStart(ctx, env(`{`+strings.ReplaceAll(common, "%s", "sessionStart")+`,"session_id":"sess_ignored","composer_mode":"agent"}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/a.go", "package src\n")
	hookruntest.WriteFile(t, root, "src/b.go", "package src\n")
	if err := fileEdit(ctx, env(`{`+strings.ReplaceAll(common, "%s", "afterFileEdit")+`,"file_path":"`+filepath.Join(root, "src", "a.go")+`","edits":[{"old_string":"","new_string":"x"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := fileEdit(ctx, env(`{`+strings.ReplaceAll(common, "%s", "afterFileEdit")+`,"file_path":"src/b.go"}`)); err != nil {
		t.Fatal(err)
	}

	if _, err := gitx.Git(ctx, root, "add", "src"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("agent work\n"), 0o644)
	commitEnv := hookrun.Env{Now: time.Now(), Cwd: root, Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp, Version: "test"}
	if err := hookrun.PrepareCommitMsg(ctx, commitEnv); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if !strings.Contains(string(data), "Agent-Session-Id: conv_7f3") || !strings.Contains(string(data), "Agent-Tool: cursor") {
		t.Fatalf("commit not stamped for Cursor:\n%s", data)
	}
	if strings.Contains(string(data), "sess_ignored") {
		t.Fatalf("the session was keyed on session_id, which file edits never carry:\n%s", data)
	}

	if err := sessionEnd(ctx, env(`{`+strings.ReplaceAll(common, "%s", "sessionEnd")+`,"reason":"completed","duration_ms":1200}`)); err != nil {
		t.Fatal(err)
	}
	if events, _, _ := sp.Pending(); events < 4 {
		t.Fatalf("expected start, two edits and an end in the spool, got %d", events)
	}
}

func TestCursorHandlersIgnoreBadInput(t *testing.T) {
	ctx := context.Background()
	for _, stdin := range []string{"", "not json", `{"hook_event_name":"sessionStart"}`, `{"conversation_id":"../../etc"}`} {
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
		if err := postToolUse(ctx, env); err != nil {
			t.Errorf("tool(%q) = %v", stdin, err)
		}
		if err := postToolUseFailure(ctx, env); err != nil {
			t.Errorf("tool failure(%q) = %v", stdin, err)
		}
	}
}

func testEnv(t *testing.T) hookrun.Env {
	root, sp := hookruntest.Project(t)
	return hookrun.Env{Now: time.Now(), Cwd: root, Spool: sp, Version: "test", Policy: config.DefaultPolicy()}
}

// The reader refuses a payload past the bound by name.
func TestReaderRefusesOversizedInput(t *testing.T) {
	const valid = `{"conversation_id":"valid"}`
	if _, err := readCursorInput(strings.NewReader(valid)); err != nil {
		t.Fatalf("a payload within the bound was refused: %v", err)
	}
	if _, err := readCursorInput(strings.NewReader(valid + strings.Repeat(" ", hookrun.MaxInput))); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized payload: err = %v, want too large", err)
	}
}
