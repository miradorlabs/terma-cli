package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// sessionStart reports a spawned thread's parent from its child rollout.
func TestCodexSubagentHooksAndSpawnedThreadParent(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string) hookrun.Env {
		return hookrun.Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	const child = "01a04490-7a2c-7b1e-8000-000000000002"
	const parent = "01a04490-7a2c-7b1e-8000-000000000001"

	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	rollout := filepath.Join(home, "sessions", "2026", "09", "16", "rollout-2026-09-16T10-00-00-"+child+".jsonl")
	_ = os.MkdirAll(filepath.Dir(rollout), 0o755)
	meta := `{"timestamp":"2026-09-16T10:00:00.000Z","type":"session_meta","payload":{"id":"` + child + `","cwd":"` + root + `","source":{"subagent":{"thread_spawn":{"agent_nickname":"Boyle","agent_path":"/root/architect","agent_role":null,"depth":1,"parent_thread_id":"` + parent + `"}}}}}` + "\n"
	if err := os.WriteFile(rollout, []byte(meta+`{"type":"event_msg","payload":{"type":"user_message"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := sessionStart(ctx, env(`{"session_id":"`+child+`","hook_event_name":"SessionStart","cwd":"`+root+`","model":"gpt-6","source":"startup","permission_mode":"default","transcript_path":"`+rollout+`"}`)); err != nil {
		t.Fatal(err)
	}
	if err := sessionStart(ctx, env(`{"session_id":"`+parent+`","hook_event_name":"SessionStart","cwd":"`+root+`","model":"gpt-6","source":"startup","permission_mode":"default","transcript_path":null}`)); err != nil {
		t.Fatal(err)
	}
	const agent = `"agent_id":"agent_7","agent_type":"reviewer","turn_id":"turn_3","model":"gpt-6","permission_mode":"default","transcript_path":null`
	if err := subagentStart(ctx, env(`{"session_id":"`+parent+`","hook_event_name":"SubagentStart","cwd":"`+root+`",`+agent+`}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/a.go", "package src\n")
	patch := "apply_patch <<'PATCH'\\n*** Begin Patch\\n*** Update File: src/a.go\\n@@\\n-old\\n+new\\n*** End Patch\\nPATCH"
	if err := postToolUse(ctx, env(`{"session_id":"`+parent+`","hook_event_name":"PostToolUse","cwd":"`+root+`",`+agent+`,"tool_name":"apply_patch","tool_input":{"command":"`+patch+`"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := subagentStop(ctx, env(`{"session_id":"`+parent+`","hook_event_name":"SubagentStop","cwd":"`+root+`",`+agent+`,"agent_transcript_path":null,"last_assistant_message":"secret","stop_hook_active":false}`)); err != nil {
		t.Fatal(err)
	}

	events := hookruntest.Lifecycle(hookruntest.Spooled(t, sp))
	if got := hookruntest.Names(events); got != "terma.session.start terma.session.start terma.subagent.start terma.files.touched terma.subagent.end" {
		t.Fatalf("events: %s", got)
	}
	if events[0].SessionID != child || events[0].Attrs["parent_session_id"] != parent || hookruntest.Num(events[0].Attrs["agent_depth"]) != 1 || events[0].Attrs["agent_nickname"] != "Boyle" || events[0].Attrs["agent_path"] != "/root/architect" {
		t.Fatalf("spawned thread start: %v", events[0].Attrs)
	}
	if _, ok := events[1].Attrs["parent_session_id"]; ok {
		t.Fatalf("root thread reports a parent: %v", events[1].Attrs)
	}
	for _, e := range events[2:] {
		if e.SessionID != parent || e.Attrs["agent_id"] != "agent_7" || e.Attrs["agent_type"] != "reviewer" || e.Attrs["tool"] != "codex" {
			t.Fatalf("%s lacks the agent facet: %v", e.Name, e.Attrs)
		}
		if _, ok := e.Attrs["last_assistant_message"]; ok {
			t.Fatalf("%s carries the assistant text", e.Name)
		}
	}
	if events[2].Attrs["turn_id"] != "turn_3" || events[2].Attrs["model"] != "gpt-6" || events[3].Attrs["files"] != "src/a.go" {
		t.Fatalf("attrs: %v / %v", events[2].Attrs, events[3].Attrs)
	}
}

// A spawned thread's hooks keep the root's session_id, name the child as agent_id, and
// point transcript_path at the child's own rollout.
func TestCodexSpawnedThreadArrivesThroughSubagentStart(t *testing.T) {
	root := hookruntest.InitRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	env := func(stdin string) hookrun.Env {
		return hookrun.Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	const rootThread = "01a04490-7a2c-7b1e-8000-00000000000a"
	const child = "01a04490-7a2c-7b1e-8000-00000000000b"

	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	rollout := filepath.Join(home, "sessions", "2026", "09", "21", "rollout-2026-09-21T10-00-00-"+child+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"timestamp":"2026-09-21T10:00:00.000Z","type":"session_meta","payload":{"id":"` + child + `","cwd":"` + root + `","source":{"subagent":{"thread_spawn":{"agent_nickname":"Holt","agent_path":"/root/reviewer","depth":1,"parent_thread_id":"` + rootThread + `"}}}}}` + "\n"
	quota := `{"timestamp":"2026-09-21T10:00:05.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_tokens":10},"rate_limits":{"plan_type":"team","primary":{"used_percent":12,"window_minutes":300,"resets_at":1800000000}}}}` + "\n"
	if err := os.WriteFile(rollout, []byte(meta+quota), 0o600); err != nil {
		t.Fatal(err)
	}
	hook := func(event, extra string) string {
		return `{"session_id":"` + rootThread + `","hook_event_name":"` + event + `","cwd":"` + root + `","agent_id":"` + child + `","agent_type":"reviewer","turn_id":"turn_1","model":"gpt-6","transcript_path":"` + rollout + `"` + extra + `}`
	}

	if err := subagentStart(ctx, env(hook("SubagentStart", ""))); err != nil {
		t.Fatal(err)
	}
	if err := subagentStop(ctx, env(hook("SubagentStop", `,"stop_hook_active":false`))); err != nil {
		t.Fatal(err)
	}
	if err := stop(ctx, env(hook("Stop", `,"stop_hook_active":false`))); err != nil {
		t.Fatal(err)
	}

	all := hookruntest.Spooled(t, sp)
	var start *spool.Event
	var quotas []spool.Event
	for i, e := range all {
		switch e.Name {
		case hookrun.EventSubagentStart:
			start = &all[i]
		case hookrun.EventSessionQuota:
			quotas = append(quotas, e)
		case hookrun.EventSessionCapture:
			t.Errorf("capture reported a problem with a rollout it should have read: %v", e.Attrs)
		}
	}
	if start == nil {
		t.Fatalf("no %s in %s", hookrun.EventSubagentStart, hookruntest.Names(all))
	}
	if start.SessionID != rootThread || start.Attrs[hookrun.AttrAgentID] != child || start.Attrs["rollout_status"] != hookrun.StatusPresent ||
		start.Attrs[hookrun.AttrAgentParentID] != rootThread || hookruntest.Num(start.Attrs["agent_depth"]) != 1 ||
		start.Attrs["agent_nickname"] != "Holt" || start.Attrs["agent_path"] != "/root/reviewer" {
		t.Fatalf("spawn record on the start: %v", start.Attrs)
	}
	if _, ok := start.Attrs[hookrun.AttrParentSession]; ok {
		t.Error("the child is a facet of the root's session, not a session with a parent")
	}
	// Read as the session's rollout this was a session_mismatch and no quota at all.
	if len(quotas) != 1 {
		t.Fatalf("quota events = %d, want the child's one: %s", len(quotas), hookruntest.Names(all))
	}
	q := quotas[0]
	if q.SessionID != rootThread || q.Attrs[hookrun.AttrAgentID] != child || q.Attrs[hookrun.AttrEvidenceStatus] != hookrun.StatusPresent || hookruntest.Num(q.Attrs["primary_used_pct"]) != 12 {
		t.Fatalf("quota from inside the subagent: session=%s attrs=%v", q.SessionID, q.Attrs)
	}
}

// A transcript that is not the named agent's is the session's: only the file name decides.
func TestCodexRolloutIDFollowsTheTranscriptName(t *testing.T) {
	for _, tc := range []struct{ name, agent, transcript, want string }{
		{"the child's rollout", "child-1", "/h/sessions/2026/09/21/rollout-2026-09-21T10-00-00-child-1.jsonl", "child-1"},
		{"the root's rollout, with an agent named", "child-1", "/h/sessions/2026/09/21/rollout-2026-09-21T10-00-00-root-1.jsonl", "root-1"},
		{"no transcript", "child-1", "", "root-1"},
		{"no agent", "", "/h/sessions/rollout-x-root-1.jsonl", "root-1"},
		{"an agent id that is not safe to match on", "../x", "/h/sessions/rollout-../x.jsonl", "root-1"},
	} {
		in := &codexHookInput{SessionID: "root-1", AgentID: tc.agent, TranscriptPath: tc.transcript}
		if got := codexRolloutID(in); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
