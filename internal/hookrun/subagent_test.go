package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hookrun/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// A Claude Code subagent is a facet of the parent session: the same session_id on
// every event, the agent named on its start, its edits and its end.
func TestClaudeSubagentIsAFacetOfTheParentSession(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string) Env {
		return Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	parent := `"session_id":"sess-claude-9","cwd":"` + root + `","prompt_id":"prompt-1"`
	const agent = `"agent_id":"a44816aa66a297cdd","agent_type":"Explore"`

	if err := SubagentStart(ctx, env(`{`+parent+`,"hook_event_name":"SubagentStart",`+agent+`,"transcript_path":"/nope"}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/sub.go", "package src\n")
	if err := PostToolUse(ctx, env(`{`+parent+`,"hook_event_name":"PostToolUse",`+agent+`,"tool_name":"Write","tool_input":{"file_path":"src/sub.go"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := SubagentStop(ctx, env(`{`+parent+`,"hook_event_name":"SubagentStop",`+agent+`,"agent_transcript_path":"/nope","last_assistant_message":"secret","stop_hook_active":false}`)); err != nil {
		t.Fatal(err)
	}
	// Without an agent_id there is no subagent to report.
	if err := SubagentStop(ctx, env(`{`+parent+`,"hook_event_name":"SubagentStop"}`)); err != nil {
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
	if err := PrepareCommitMsg(ctx, Env{Now: time.Now(), Cwd: root, Args: []string{msgPath, "message"}, Stdin: strings.NewReader(""), Spool: sp}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(msgPath)
	if got := trailer.Parse(string(data), "#"); len(got) != 1 || got[0].SessionID != "sess-claude-9" {
		t.Fatalf("unexpected trailers %+v in:\n%s", got, data)
	}
}

// Codex has both shapes: in-thread subagents (hooks with agent_id) and spawned threads
// (a child rollout naming its parent), and CodexSessionStart reports the latter.
func TestCodexSubagentHooksAndSpawnedThreadParent(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := func(stdin string) Env {
		return Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
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

	if err := CodexSessionStart(ctx, env(`{"session_id":"`+child+`","hook_event_name":"SessionStart","cwd":"`+root+`","model":"gpt-6","source":"startup","permission_mode":"default","transcript_path":"`+rollout+`"}`)); err != nil {
		t.Fatal(err)
	}
	// A root thread: no rollout to read, no parent to report.
	if err := CodexSessionStart(ctx, env(`{"session_id":"`+parent+`","hook_event_name":"SessionStart","cwd":"`+root+`","model":"gpt-6","source":"startup","permission_mode":"default","transcript_path":null}`)); err != nil {
		t.Fatal(err)
	}
	const agent = `"agent_id":"agent_7","agent_type":"reviewer","turn_id":"turn_3","model":"gpt-6","permission_mode":"default","transcript_path":null`
	if err := CodexSubagentStart(ctx, env(`{"session_id":"`+parent+`","hook_event_name":"SubagentStart","cwd":"`+root+`",`+agent+`}`)); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, root, "src/a.go", "package src\n")
	patch := "apply_patch <<'PATCH'\\n*** Begin Patch\\n*** Update File: src/a.go\\n@@\\n-old\\n+new\\n*** End Patch\\nPATCH"
	if err := CodexPostToolUse(ctx, env(`{"session_id":"`+parent+`","hook_event_name":"PostToolUse","cwd":"`+root+`",`+agent+`,"tool_name":"apply_patch","tool_input":{"command":"`+patch+`"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := CodexSubagentStop(ctx, env(`{"session_id":"`+parent+`","hook_event_name":"SubagentStop","cwd":"`+root+`",`+agent+`,"agent_transcript_path":null,"last_assistant_message":"secret","stop_hook_active":false}`)); err != nil {
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

// A Codex subagent is a thread the session spawned, with a rollout of its own. Its hooks
// keep the root's session_id, carry the child thread's id as agent_id, and point
// transcript_path at the child's rollout.
func TestCodexSpawnedThreadArrivesThroughSubagentStart(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	env := func(stdin string) Env {
		return Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
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

	if err := CodexSubagentStart(ctx, env(hook("SubagentStart", ""))); err != nil {
		t.Fatal(err)
	}
	// The end of the child's turn: capture runs against the rollout the hook names.
	if err := CodexSubagentStop(ctx, env(hook("SubagentStop", `,"stop_hook_active":false`))); err != nil {
		t.Fatal(err)
	}
	if err := CodexStop(ctx, env(hook("Stop", `,"stop_hook_active":false`))); err != nil {
		t.Fatal(err)
	}

	all := hookruntest.Spooled(t, sp)
	var start *spool.Event
	var quotas []spool.Event
	for i, e := range all {
		switch e.Name {
		case EventSubagentStart:
			start = &all[i]
		case EventSessionQuota:
			quotas = append(quotas, e)
		case EventSessionCapture:
			t.Errorf("capture reported a problem with a rollout it should have read: %v", e.Attrs)
		}
	}
	if start == nil {
		t.Fatalf("no %s in %s", EventSubagentStart, hookruntest.Names(all))
	}
	if start.SessionID != rootThread || start.Attrs[AttrAgentID] != child || start.Attrs["rollout_status"] != StatusPresent ||
		start.Attrs[AttrAgentParentID] != rootThread || hookruntest.Num(start.Attrs["agent_depth"]) != 1 ||
		start.Attrs["agent_nickname"] != "Holt" || start.Attrs["agent_path"] != "/root/reviewer" {
		t.Fatalf("spawn record on the start: %v", start.Attrs)
	}
	if _, ok := start.Attrs[AttrParentSession]; ok {
		t.Error("the child is a facet of the root's session, not a session with a parent")
	}
	// Read as the session's rollout this was a session_mismatch and no quota at all.
	if len(quotas) != 1 {
		t.Fatalf("quota events = %d, want the child's one: %s", len(quotas), hookruntest.Names(all))
	}
	q := quotas[0]
	if q.SessionID != rootThread || q.Attrs[AttrAgentID] != child || q.Attrs[AttrEvidenceStatus] != StatusPresent || hookruntest.Num(q.Attrs["primary_used_pct"]) != 12 {
		t.Fatalf("quota from inside the subagent: session=%s attrs=%v", q.SessionID, q.Attrs)
	}
}

// A hook that names an agent and a transcript that is not that agent's is the session's
// rollout still: only the file's own name says whose it is.
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
