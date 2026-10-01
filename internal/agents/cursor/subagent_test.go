package cursor

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

// Cursor reports a finished subagent under the parent conversation, with the files it
// modified joining that conversation's manifest.
func TestCursorSubagentStopRecordsOutcomeAndFiles(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	elsewhere := t.TempDir()
	env := func(stdin string) hookrun.Env {
		return hookrun.Env{Now: time.Now(), Cwd: elsewhere, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	common := `"conversation_id":"conv_9a1","model":"claude-opus-5","workspace_roots":["` + root + `"]`
	if err := sessionStart(ctx, env(`{`+common+`,"hook_event_name":"sessionStart"}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/a.go", "package src\n")
	hookruntest.WriteFile(t, root, "src/b.go", "package src\n")
	stop := `{` + common + `,"hook_event_name":"subagentStop","subagent_type":"explorer","status":"completed","task":"look around","summary":"done","duration_ms":4200,"message_count":6,"tool_call_count":3,"loop_count":0,"modified_files":["` + filepath.Join(root, "src", "a.go") + `","src/b.go","/elsewhere/c.go"],"agent_transcript_path":"/nope"}`
	if err := subagentStop(ctx, env(stop)); err != nil {
		t.Fatal(err)
	}
	if err := subagentStop(ctx, env(`{`+common+`,"hook_event_name":"subagentStop","subagent_type":"worker","status":"weird","modified_files":[]}`)); err != nil {
		t.Fatal(err)
	}

	events := hookruntest.Lifecycle(hookruntest.Spooled(t, sp))
	if got := hookruntest.Names(events); got != "terma.session.start terma.files.touched terma.subagent.end terma.subagent.end" {
		t.Fatalf("events: %s", got)
	}
	touched, end, bare := events[1], events[2], events[3]
	if touched.SessionID != "conv_9a1" || touched.Attrs["files"] != "src/a.go,src/b.go" || hookruntest.Num(touched.Attrs["file_count"]) != 2 || touched.Attrs["tool_name"] != "subagentStop" || touched.Attrs["agent_type"] != "explorer" {
		t.Fatalf("touched: %v", touched.Attrs)
	}
	if end.Attrs["agent_type"] != "explorer" || end.Attrs["status"] != "completed" || hookruntest.Num(end.Attrs["duration_ms"]) != 4200 || hookruntest.Num(end.Attrs["message_count"]) != 6 || hookruntest.Num(end.Attrs["tool_call_count"]) != 3 || end.Attrs["loop_count"] != 0.0 || hookruntest.Num(end.Attrs["file_count"]) != 2 {
		t.Fatalf("end: %v", end.Attrs)
	}
	for _, k := range []string{"task", "summary", "agent_transcript_path", "files"} {
		if _, ok := end.Attrs[k]; ok {
			t.Fatalf("end carries %s", k)
		}
	}
	if bare.Attrs["status"] != "unknown" || bare.Attrs["file_count"] != 0.0 {
		t.Fatalf("bare end: %v", bare.Attrs)
	}

	if _, err := gitx.Git(ctx, root, "add", "src"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("subagent work\n"), 0o644)
	if err := hookrun.PrepareCommitMsg(ctx, hookrun.Env{Now: time.Now(), Cwd: root, Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(msgPath); !strings.Contains(string(data), "Agent-Session-Id: conv_9a1") {
		t.Fatalf("the subagent's files did not stamp the conversation's commit:\n%s", data)
	}
}

// cursor-agent can file a subagent's own afterFileEdit under the subagent's conversation
// id. Left there, the commit is stamped with a session nobody can open — or, once
// subagentStop adds the same files to the parent, with two.
func TestCursorSubagentEditsAreFoldedIntoTheParentConversation(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string, args ...string) hookrun.Env {
		return hookrun.Env{Now: time.Now(), Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	const parent, child = "conv_parent_1", "conv_child_7"
	roots := `"workspace_roots":["` + root + `"],"model":"claude-opus-5"`

	if err := sessionStart(ctx, env(`{"conversation_id":"`+parent+`",`+roots+`,"hook_event_name":"sessionStart"}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/sub.go", "package src\n")
	// The subagent's edit, under the subagent's own conversation.
	if err := fileEdit(ctx, env(`{"conversation_id":"`+child+`",`+roots+`,"hook_event_name":"afterFileEdit","file_path":"`+filepath.Join(root, "src", "sub.go")+`"}`)); err != nil {
		t.Fatal(err)
	}
	stop := `{"conversation_id":"` + child + `","parent_conversation_id":"` + parent + `","subagent_id":"` + child + `",` + roots +
		`,"hook_event_name":"subagentStop","subagent_type":"worker","status":"completed","generation_id":"gen_4","modified_files":["src/sub.go"]}`
	if err := subagentStop(ctx, env(stop)); err != nil {
		t.Fatal(err)
	}

	manifests, err := hookruntest.Store(t, root).Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 || manifests[0].SessionID != parent {
		t.Fatalf("manifests = %+v, want only the parent conversation's", manifests)
	}

	var end, touched *spool.Event
	events := hookruntest.Lifecycle(hookruntest.Spooled(t, sp))
	for i, e := range events {
		switch {
		case e.Name == hookrun.EventSubagentEnd:
			end = &events[i]
		case e.Name == hookrun.EventFilesTouched && e.Attrs[hookrun.AttrToolName] == "subagentStop":
			touched = &events[i]
		}
	}
	if end == nil || end.SessionID != parent || end.Attrs[hookrun.AttrAgentID] != child || end.Attrs[hookrun.AttrAgentType] != "worker" {
		t.Fatalf("subagent end: %+v", end)
	}
	if touched == nil || touched.SessionID != parent || touched.Attrs[hookrun.AttrAgentID] != child || touched.Attrs[hookrun.AttrTurnID] != "gen_4" || touched.Attrs["files"] != "src/sub.go" {
		t.Fatalf("files touched: %+v", touched)
	}

	// The commit carries one trailer, and it is the conversation a person can find.
	if _, err := gitx.Git(ctx, root, "add", "src/sub.go"); err != nil {
		t.Fatal(err)
	}
	msg := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(msg, []byte("Add sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := hookrun.PrepareCommitMsg(ctx, env("", msg, "message")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msg)
	if got := trailer.Parse(string(data), "#"); len(got) != 1 || got[0].SessionID != parent {
		t.Fatalf("trailers = %+v, want exactly the parent's:\n%s", got, data)
	}
}

// A subagent hook that names only the conversation that spawned it is filed there.
func TestCursorHookWithOnlyAParentConversationIsTheParents(t *testing.T) {
	in, err := readCursorInput(strings.NewReader(`{"parent_conversation_id":"conv_parent_1","hook_event_name":"subagentStop"}`))
	if err != nil || in.id() != "conv_parent_1" {
		t.Fatalf("id = %q, err = %v", in.id(), err)
	}
	if _, err := readCursorInput(strings.NewReader(`{"parent_conversation_id":"../escape","hook_event_name":"subagentStop"}`)); err == nil {
		t.Fatal("an unsafe parent id must not stand in for a missing conversation id")
	}
}
