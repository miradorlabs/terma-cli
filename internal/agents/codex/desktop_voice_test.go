package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// testdata/codex_desktop_voice_rollout.jsonl is the start of a real Codex Desktop
// 0.155.0-alpha.16.3 voice chat, its text redacted: one turn, still running, with 40
// replies among realtime transcript and delegated-task records. In production that turn ran
// for hours with no Stop; the one capture at its end read a single batch of 32 replies, so
// the name Codex gave the thread never left.
const (
	voiceThread = "01a116c1-c3b9-7e01-b3aa-7a3111ff2050"
	voiceTitle  = `{"id":"01a116c1-c3b9-7e01-b3aa-7a3111ff2050","thread_name":"New voice chat","updated_at":"2026-10-07T14:26:01.351202Z"}`
)

func voiceRollout(t *testing.T, cwd string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "codex_desktop_voice_rollout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(cwd)
	dir := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "10", "07")
	name := "rollout-2026-10-07T15-25-59-" + voiceThread + ".jsonl"
	hookruntest.WriteFile(t, dir, name, strings.ReplaceAll(string(b), `"/work/terma"`, string(quoted)))
	titleIndex(t, voiceTitle)
	return filepath.Join(dir, name)
}

func runVoiceHook(t *testing.T, env hookrun.Env, handler func(context.Context, hookrun.Env) error, path string) (replies, titles []spool.Event) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"session_id": voiceThread, "cwd": env.Cwd, "transcript_path": path, "model": "gpt-6-astra"})
	env.Stdin = strings.NewReader(string(in))
	if err := handler(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	all := hookruntest.Spooled(t, env.Spool)
	return hookruntest.Named(all, semconv.TermaAssistantMessageEvent), hookruntest.Named(all, semconv.TermaSessionTitleEvent)
}

// One capture reads the whole backlog, so the thread is named at its first hook however
// many replies came before it.
func TestCodexDesktopVoiceThreadIsNamedAtItsFirstCapture(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t, &env)
	path := voiceRollout(t, env.Cwd)
	replies, titles := runVoiceHook(t, env, stop, path)
	if len(replies) != 40 {
		t.Fatalf("spooled %d replies, want all 40", len(replies))
	}
	if len(titles) != 1 || titles[0].Attrs[semconv.TermaSessionTitleKey] != "New voice chat" || titles[0].SessionID != voiceThread {
		t.Fatalf("titles %+v", titles)
	}
}

// A voice turn has no Stop for hours; its tool calls name the thread meanwhile.
func TestCodexDesktopVoiceThreadIsNamedBeforeItsTurnEnds(t *testing.T) {
	env := fundingEnv(t)
	routeCodex(t, &env)
	path := voiceRollout(t, env.Cwd)
	replies, titles := runVoiceHook(t, env, postToolUse, path)
	if len(replies) != 40 || len(titles) != 1 {
		t.Fatalf("PostToolUse spooled %d replies and %d titles, want 40 and 1", len(replies), len(titles))
	}
	// The Stop that follows sends neither again.
	if replies, titles = runVoiceHook(t, env, stop, path); len(replies) != 0 || len(titles) != 0 {
		t.Fatalf("Stop sent %d replies and %d titles again", len(replies), len(titles))
	}
}

// The thread keeps its name home once a turn ran where the team does not collect, however
// far into the backlog that turn is.
func TestCodexDesktopVoiceThreadUnlistedTurnKeepsTheName(t *testing.T) {
	other := hookruntest.InitRepo(t)
	env := fundingEnv(t)
	routeCodex(t, &env)
	path := voiceRollout(t, env.Cwd)
	appendRollout(t, path, `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"}}`+"\n"+turnContext("turn-2", other))
	if _, titles := runVoiceHook(t, env, stop, path); len(titles) != 0 {
		t.Fatalf("the name left after a turn the team does not collect: %+v", titles)
	}
}
