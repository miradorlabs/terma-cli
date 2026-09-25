package hookrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Claude's internal summary forks emit SubagentStop with a fresh id, without
// SubagentStart or an Agent call. A custom --agent can supply a nonempty type too.
func TestClaudeSubagentStopIgnoresInternalForks(t *testing.T) {
	root := initRepo(t)
	sp, _ := spool.Open(t.TempDir())
	for i := range 61 {
		kind := ""
		if i%2 == 0 {
			kind = "custom-reviewer"
		}
		payload := fmt.Sprintf(`{"session_id":"parent","agent_id":"summary-%d","agent_type":%q,"hook_event_name":"SubagentStop","last_assistant_message":"NEVER-READ-REPLY"}`, i, kind)
		env := Env{Cwd: root, Spool: sp, Now: time.Now().Add(time.Duration(i) * 30 * time.Second), Stdin: strings.NewReader(payload)}
		if err := SubagentStop(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	if events := drain(t, sp); len(events) != 0 {
		t.Fatalf("internal forks produced %d events, want none", len(events))
	}
}

func TestClaudeSubagentStopRequiresEvidenceForItsSessionAndAgent(t *testing.T) {
	for _, launch := range []string{"start", "Agent", "Task"} {
		t.Run(launch, func(t *testing.T) {
			root := initRepo(t)
			sp, _ := spool.Open(t.TempDir())
			ctx := context.Background()
			const sessionID = "sess-agent-1"
			const agentID = "a838402ff1ef43bee"
			hook := func(handler func(context.Context, Env) error, sessionID, agentID string) {
				t.Helper()
				payload := fmt.Sprintf(`{"session_id":%q,"agent_id":%q,"agent_type":"custom-reviewer"}`, sessionID, agentID)
				if err := handler(ctx, Env{Cwd: root, Spool: sp, Stdin: strings.NewReader(payload)}); err != nil {
					t.Fatal(err)
				}
			}
			if launch == "start" {
				hook(SubagentStart, sessionID, agentID)
			} else {
				runPostToolUse(t, root, sp, agentToolPayload(root, launch, launchedAgentResponse, ""))
			}
			// Delivery removes the launch from the spool; later hook processes must
			// still recognize the agent, including on another turn or a repeated stop.
			if events := drain(t, sp); len(events) != 1 {
				t.Fatalf("launch produced %d events, want one", len(events))
			}
			hook(SubagentStop, "another-session", agentID)
			hook(SubagentStop, sessionID, "internal-fork")
			hook(SubagentStop, sessionID, agentID)
			hook(SubagentStop, sessionID, agentID)
			events := drain(t, sp)
			if len(events) != 2 {
				t.Fatalf("got %d stop events, want the two known-agent stops", len(events))
			}
			for _, event := range events {
				if event.Name != EventSubagentEnd || event.SessionID != sessionID || event.Attrs[attrAgentID] != agentID {
					t.Errorf("unexpected stop: %+v", event)
				}
			}
		})
	}
}

func TestClaudeSubagentConcurrentLaunchesSurvive(t *testing.T) {
	root := initRepo(t)
	sp, _ := spool.Open(t.TempDir())
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			payload := fmt.Sprintf(`{"session_id":"parent","agent_id":"worker-%d"}`, i)
			if err := SubagentStart(ctx, Env{Cwd: root, Spool: sp, Stdin: strings.NewReader(payload)}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	// Only the later stop events matter here; launch evidence must survive a flush.
	drain(t, sp)
	for i := range 16 {
		payload := fmt.Sprintf(`{"session_id":"parent","agent_id":"worker-%d"}`, i)
		if err := SubagentStop(ctx, Env{Cwd: root, Spool: sp, Stdin: strings.NewReader(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	if events := drain(t, sp); len(events) != 16 {
		t.Fatalf("concurrent launches left %d recognized agents, want 16", len(events))
	}
}

func TestClaudeSubagentLaunchEvidenceExpires(t *testing.T) {
	root := initRepo(t)
	sp, _ := spool.Open(t.TempDir())
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"old", "fresh"} {
		payload := fmt.Sprintf(`{"session_id":"parent","agent_id":%q}`, id)
		if err := SubagentStart(ctx, Env{Cwd: root, Spool: sp, Now: now, Stdin: strings.NewReader(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	drain(t, sp)
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
		if err := SubagentStop(ctx, Env{Cwd: root, Spool: sp, Now: now, Stdin: strings.NewReader(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	if events := drain(t, sp); len(events) != 1 || events[0].Attrs[attrAgentID] != "fresh" {
		t.Fatalf("expired evidence admitted a stop: %+v", events)
	}
	if err := SessionStart(ctx, Env{Cwd: root, Spool: sp, Now: now, Stdin: strings.NewReader(`{"session_id":"next-session"}`)}); err != nil {
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
	root := initRepo(t)
	sp, _ := spool.Open(t.TempDir())
	path, err := claudeSubagentPath("parent", "worker")
	if err != nil {
		t.Fatal(err)
	}
	// A file in place of the state directory makes recording a launch fail.
	if err := os.WriteFile(filepath.Dir(path), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, handler := range []func(context.Context, Env) error{SubagentStart, SubagentStop} {
		env := Env{Cwd: root, Spool: sp, Stdin: strings.NewReader(`{"session_id":"parent","agent_id":"worker"}`)}
		if err := handler(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	if events := drain(t, sp); len(events) != 1 || events[0].Name != EventSubagentStart {
		t.Fatalf("want the launch alone when its evidence cannot be saved: %+v", events)
	}
}
