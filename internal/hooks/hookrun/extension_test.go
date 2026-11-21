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
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// An extension's session is announced, names its edits, stamps the next commit and ends;
// a prompt, or a missing or unsafe session id, records nothing.
func TestExtensionSessionIsStampedOnItsCommit(t *testing.T) {
	t.Parallel()
	root, stateDir, sp := hookruntest.Project(t)
	ctx := context.Background()
	env := func(stdin string, args ...string) Env {
		return Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test", Team: hookruntest.Team}
	}
	events := Extension{Tool: "ext"}.Events("ext")
	for _, name := range []string{"ext-session-start", "ext-prompt", "ext-session-end", "ext-file-edit"} {
		if events[name] == nil {
			t.Fatalf("no %s handler", name)
		}
	}
	const sid = `"session_id":"7c9e6679-7425-40de-944b-e07fc1f90ae7"`
	for _, stdin := range []string{`{"cwd":"` + hookruntest.InJSON(root) + `"}`, `{"session_id":"../escape","cwd":"` + hookruntest.InJSON(root) + `"}`} {
		_ = events["ext-session-start"](ctx, env(stdin))
	}
	if n := len(hookruntest.Spooled(t, sp)); n != 0 {
		t.Fatalf("a payload without a safe session spooled %d events", n)
	}
	if err := events["ext-session-start"](ctx, env(`{`+sid+`,"cwd":"`+hookruntest.InJSON(root)+`","model":"m1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := events["ext-prompt"](ctx, env(`{`+sid+`}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/a.go", "package src\n")
	if err := events["ext-file-edit"](ctx, env(`{`+sid+`,"cwd":"`+hookruntest.InJSON(root)+`","file":"`+hookruntest.InJSON(filepath.Join(root, "src", "a.go"))+`","tool":"write"}`)); err != nil {
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
	if err := events["ext-session-end"](ctx, env(`{`+sid+`,"cwd":"`+hookruntest.InJSON(root)+`"}`)); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.Names(hookruntest.Lifecycle(hookruntest.Spooled(t, sp)))
	if got != "terma.session.start terma.files.touched terma.commit.stamped terma.session.end" {
		t.Fatalf("spooled %s", got)
	}
}

// A subagent's start names its parent: its start event carries it, its relay claim too
// where the extension claims parents, and the session is not made active. One tool call's several files are one touch, joined to
// the call by its id.
func TestExtensionSubagentAndMultiFileEdit(t *testing.T) {
	t.Parallel()
	root, stateDir, sp := hookruntest.Project(t)
	ctx := context.Background()
	now := time.Now()
	env := func(stdin string) Env {
		return Env{StateDir: stateDir, Now: now, Cwd: root, Policy: hookruntest.Admitting(root), Stdin: strings.NewReader(stdin), Spool: sp, Version: "test", Team: hookruntest.Team}
	}
	events := Extension{Tool: "ext", ClaimParent: true}.Events("ext")
	const child, parent = "01a1fe3e-6e03-7557-b1af-a171883bcfca", "01a1fe3e-5eb1-7520-a3b3-a7f2a87931bf"
	if err := events["ext-session-start"](ctx, env(`{"session_id":"`+child+`","cwd":"`+hookruntest.InJSON(root)+`","parent_session_id":"`+parent+`"}`)); err != nil {
		t.Fatal(err)
	}
	start := hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaSessionStartEvent)
	if len(start) != 1 || start[0].Attrs[semconv.TermaSessionParentIDKey] != parent {
		t.Fatalf("start = %+v, want one naming parent %s", start, parent)
	}
	if c, ok := claim.Read(stateDir, child, now); !ok || c.Parent != parent {
		t.Fatalf("claim = %+v, %v; want it to name parent %s", c, ok, parent)
	}
	if active, ok := hookruntest.Store(t, root).Active(now, time.Hour); ok {
		t.Fatalf("a subagent's session was made active: %+v", active)
	}
	// An agent whose own telemetry names the parent leaves it out of the claim, so the relay
	// stamps nothing on that agent's records.
	const other = "01a1fe3e-6e03-7557-b1af-a171883bcfcb"
	if err := (Extension{Tool: "other"}).Events("other")["other-session-start"](ctx, env(`{"session_id":"`+other+`","cwd":"`+hookruntest.InJSON(root)+`","parent_session_id":"`+parent+`"}`)); err != nil {
		t.Fatal(err)
	}
	if c, ok := claim.Read(stateDir, other, now); !ok || c.Parent != "" {
		t.Fatalf("claim = %+v, %v; want one naming no parent", c, ok)
	}

	hookruntest.WriteFile(t, root, "a.go", "package a\n")
	hookruntest.WriteFile(t, root, "b.go", "package a\n")
	if err := events["ext-file-edit"](ctx, env(`{"session_id":"`+child+`","cwd":"`+hookruntest.InJSON(root)+`","tool":"edit","files":["`+
		hookruntest.InJSON(filepath.Join(root, "a.go"))+`","b.go"],"tool_call_id":"h3DEyzO37|fc_tmp_avf6zkfc1t"}`)); err != nil {
		t.Fatal(err)
	}
	touched := hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaFilesTouchedEvent)
	if len(touched) != 1 || touched[0].Attrs[semconv.GenAIToolCallIDKey] != "h3DEyzO37|fc_tmp_avf6zkfc1t" ||
		hookruntest.Joined(touched[0].Attrs[semconv.TermaFilesPathsKey]) != "a.go,b.go" {
		t.Fatalf("touched = %+v", touched)
	}
}
