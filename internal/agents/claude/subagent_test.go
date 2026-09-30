package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hookrun/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// Claude's internal summary forks emit SubagentStop with a fresh id, without
// SubagentStart or an Agent call. A custom --agent can supply a nonempty type too.
func TestClaudeSubagentStopIgnoresInternalForks(t *testing.T) {
	root := newRepo(t)
	sp, _ := spool.Open(t.TempDir())
	for i := range 61 {
		kind := ""
		if i%2 == 0 {
			kind = "custom-reviewer"
		}
		payload := fmt.Sprintf(`{"session_id":"parent","agent_id":"summary-%d","agent_type":%q,"hook_event_name":"SubagentStop","last_assistant_message":"NEVER-READ-REPLY"}`, i, kind)
		env := hookrun.Env{Cwd: root, Spool: sp, Now: time.Now().Add(time.Duration(i) * 30 * time.Second), Stdin: strings.NewReader(payload)}
		if err := subagentStop(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	if events := hookruntest.Spooled(t, sp); len(events) != 0 {
		t.Fatalf("internal forks produced %d events, want none", len(events))
	}
}

func TestClaudeSubagentStopRequiresEvidenceForItsSessionAndAgent(t *testing.T) {
	for _, launch := range []string{"start", "Agent", "Task"} {
		t.Run(launch, func(t *testing.T) {
			root := newRepo(t)
			sp, _ := spool.Open(t.TempDir())
			ctx := context.Background()
			const sessionID = "sess-agent-1"
			const agentID = "a838402ff1ef43bee"
			hook := func(handler func(context.Context, hookrun.Env) error, sessionID, agentID string) {
				t.Helper()
				payload := fmt.Sprintf(`{"session_id":%q,"agent_id":%q,"agent_type":"custom-reviewer"}`, sessionID, agentID)
				if err := handler(ctx, hookrun.Env{Cwd: root, Spool: sp, Stdin: strings.NewReader(payload)}); err != nil {
					t.Fatal(err)
				}
			}
			if launch == "start" {
				hook(subagentStart, sessionID, agentID)
			} else {
				runPostToolUse(t, root, sp, agentToolPayload(root, launch, launchedAgentResponse, ""))
			}
			// Delivery removes the launch from the spool; later hook processes must
			// still recognize the agent, including on another turn or a repeated stop.
			if events := hookruntest.Spooled(t, sp); len(events) != 1 {
				t.Fatalf("launch produced %d events, want one", len(events))
			}
			hook(subagentStop, "another-session", agentID)
			hook(subagentStop, sessionID, "internal-fork")
			hook(subagentStop, sessionID, agentID)
			hook(subagentStop, sessionID, agentID)
			events := hookruntest.Spooled(t, sp)
			if len(events) != 2 {
				t.Fatalf("got %d stop events, want the two known-agent stops", len(events))
			}
			for _, event := range events {
				if event.Name != hookrun.EventSubagentEnd || event.SessionID != sessionID || event.Attrs[hookrun.AttrAgentID] != agentID {
					t.Errorf("unexpected stop: %+v", event)
				}
			}
		})
	}
}

func TestClaudeSubagentConcurrentLaunchesSurvive(t *testing.T) {
	root := newRepo(t)
	sp, _ := spool.Open(t.TempDir())
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			payload := fmt.Sprintf(`{"session_id":"parent","agent_id":"worker-%d"}`, i)
			if err := subagentStart(ctx, hookrun.Env{Cwd: root, Spool: sp, Stdin: strings.NewReader(payload)}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	// Only the later stop events matter here; launch evidence must survive a flush.
	hookruntest.Spooled(t, sp)
	for i := range 16 {
		payload := fmt.Sprintf(`{"session_id":"parent","agent_id":"worker-%d"}`, i)
		if err := subagentStop(ctx, hookrun.Env{Cwd: root, Spool: sp, Stdin: strings.NewReader(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	if events := hookruntest.Spooled(t, sp); len(events) != 16 {
		t.Fatalf("concurrent launches left %d recognized agents, want 16", len(events))
	}
}

func TestClaudeSubagentLaunchEvidenceExpires(t *testing.T) {
	root := newRepo(t)
	sp, _ := spool.Open(t.TempDir())
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"old", "fresh"} {
		payload := fmt.Sprintf(`{"session_id":"parent","agent_id":%q}`, id)
		if err := subagentStart(ctx, hookrun.Env{Cwd: root, Spool: sp, Now: now, Stdin: strings.NewReader(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	hookruntest.Spooled(t, sp)
	path, err := claudeSubagentPath("parent", "old")
	if err != nil {
		t.Fatal(err)
	}
	expired := now.Add(-spool.MaxAge - time.Second)
	if err := os.Chtimes(path, expired, expired); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "fresh"} {
		payload := fmt.Sprintf(`{"session_id":"parent","agent_id":%q}`, id)
		if err := subagentStop(ctx, hookrun.Env{Cwd: root, Spool: sp, Now: now, Stdin: strings.NewReader(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	if events := hookruntest.Spooled(t, sp); len(events) != 1 || events[0].Attrs[hookrun.AttrAgentID] != "fresh" {
		t.Fatalf("expired evidence admitted a stop: %+v", events)
	}
	if err := sessionStart(ctx, hookrun.Env{Cwd: root, Spool: sp, Now: now, Stdin: strings.NewReader(`{"session_id":"next-session"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("old launch evidence was not pruned: %v", err)
	}
	fresh, _ := claudeSubagentPath("parent", "fresh")
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("pruned fresh launch evidence: %v", err)
	}
}

func TestClaudeSubagentUnavailableLaunchStateDoesNotFailHooks(t *testing.T) {
	root := newRepo(t)
	sp, _ := spool.Open(t.TempDir())
	path, err := claudeSubagentPath("parent", "worker")
	if err != nil {
		t.Fatal(err)
	}
	// A file in place of the state directory makes recording a launch fail.
	if err := os.WriteFile(filepath.Dir(path), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, handler := range []func(context.Context, hookrun.Env) error{subagentStart, subagentStop} {
		env := hookrun.Env{Cwd: root, Spool: sp, Stdin: strings.NewReader(`{"session_id":"parent","agent_id":"worker"}`)}
		if err := handler(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	if events := hookruntest.Spooled(t, sp); len(events) != 1 || events[0].Name != hookrun.EventSubagentStart {
		t.Fatalf("want the launch alone when its evidence cannot be saved: %+v", events)
	}
}

// A Claude Code subagent is a facet of the parent session: the same session_id on
// every event, the agent named on its start, its edits and its end.
func TestClaudeSubagentIsAFacetOfTheParentSession(t *testing.T) {
	root := newRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string) hookrun.Env {
		return hookrun.Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	parent := `"session_id":"sess-claude-9","cwd":"` + root + `","prompt_id":"prompt-1"`
	const agent = `"agent_id":"a44816aa66a297cdd","agent_type":"Explore"`

	if err := subagentStart(ctx, env(`{`+parent+`,"hook_event_name":"SubagentStart",`+agent+`,"transcript_path":"/nope"}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/sub.go", "package src\n")
	if err := postToolUse(ctx, env(`{`+parent+`,"hook_event_name":"PostToolUse",`+agent+`,"tool_name":"Write","tool_input":{"file_path":"src/sub.go"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := subagentStop(ctx, env(`{`+parent+`,"hook_event_name":"SubagentStop",`+agent+`,"agent_transcript_path":"/nope","last_assistant_message":"secret","stop_hook_active":false}`)); err != nil {
		t.Fatal(err)
	}
	// Without an agent_id there is no subagent to report.
	if err := subagentStop(ctx, env(`{`+parent+`,"hook_event_name":"SubagentStop"}`)); err != nil {
		t.Fatal(err)
	}

	events := hookruntest.Spooled(t, sp)
	if got := hookruntest.Names(events); got != "terma.subagent.start terma.files.touched terma.subagent.end" {
		t.Fatalf("events: %s", got)
	}
	for _, e := range events {
		if e.SessionID != "sess-claude-9" {
			t.Fatalf("%s keyed on %q, want the parent session", e.Name, e.SessionID)
		}
		if e.Attrs["agent_id"] != "a44816aa66a297cdd" || e.Attrs["agent_type"] != "Explore" || e.Attrs["tool"] != "claude-code" {
			t.Fatalf("%s lacks the agent facet: %v", e.Name, e.Attrs)
		}
		for _, k := range []string{"last_assistant_message", "agent_transcript_path", "transcript_path"} {
			if _, ok := e.Attrs[k]; ok {
				t.Fatalf("%s carries %s", e.Name, k)
			}
		}
	}
	if events[0].Attrs["turn_id"] != "prompt-1" || events[0].Attrs["terma.version"] != "test" {
		t.Fatalf("start attrs: %v", events[0].Attrs)
	}
	if events[1].Attrs["files"] != "src/sub.go" {
		t.Fatalf("touched attrs: %v", events[1].Attrs)
	}

	// The manifest is the session's: the parent's commit of the subagent's file is stamped.
	if _, err := gitx.Git(ctx, root, "add", "src/sub.go"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("subagent work\n"), 0o644)
	if err := hookrun.PrepareCommitMsg(ctx, hookrun.Env{Now: time.Now(), Cwd: root, Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if got := trailer.Parse(string(data), "#"); len(got) != 1 || got[0].SessionID != "sess-claude-9" {
		t.Fatalf("unexpected trailers %+v in:\n%s", got, data)
	}
}
