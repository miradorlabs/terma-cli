package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// An extension's session is announced, names its edits, stamps the next commit and ends;
// a prompt, or a missing or unsafe session id, records nothing.
func TestExtensionSessionIsStampedOnItsCommit(t *testing.T) {
	root, sp := hookruntest.Project(t)
	ctx := context.Background()
	env := func(stdin string, args ...string) Env {
		return Env{Now: time.Now(), Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	events := Extension{Tool: "ext"}.Events("ext")
	for _, name := range []string{"ext-session-start", "ext-prompt", "ext-session-end", "ext-file-edit"} {
		if events[name] == nil {
			t.Fatalf("no %s handler", name)
		}
	}
	const sid = `"session_id":"7c9e6679-7425-40de-944b-e07fc1f90ae7"`
	for _, stdin := range []string{`{"cwd":"` + root + `"}`, `{"session_id":"../escape","cwd":"` + root + `"}`} {
		_ = events["ext-session-start"](ctx, env(stdin))
	}
	if n := len(hookruntest.Spooled(t, sp)); n != 0 {
		t.Fatalf("a payload without a safe session spooled %d events", n)
	}
	if err := events["ext-session-start"](ctx, env(`{`+sid+`,"cwd":"`+root+`","model":"m1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := events["ext-prompt"](ctx, env(`{`+sid+`}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/a.go", "package src\n")
	if err := events["ext-file-edit"](ctx, env(`{`+sid+`,"cwd":"`+root+`","file":"`+filepath.Join(root, "src", "a.go")+`","tool":"write"}`)); err != nil {
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
	if data, _ := os.ReadFile(msgPath); !strings.Contains(string(data), "Agent-Session-Id: 7c9e6679-7425-40de-944b-e07fc1f90ae7") || !strings.Contains(string(data), "Agent-Tool: ext") {
		t.Fatalf("commit not stamped for the extension's session:\n%s", data)
	}
	if err := events["ext-session-end"](ctx, env(`{`+sid+`,"cwd":"`+root+`"}`)); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.Names(hookruntest.Lifecycle(hookruntest.Spooled(t, sp)))
	if got != "terma.session.start terma.files.touched terma.commit.stamped terma.session.end" {
		t.Fatalf("spooled %s", got)
	}
}
