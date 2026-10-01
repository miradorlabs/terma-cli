package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func fundingEnv(t *testing.T) hookrun.Env {
	t.Helper()
	root := hookruntest.InitRepo(t)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	// A test launched from routed Codex must not inherit its parent's consent mode.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	hookruntest.WriteFile(t, root, ".terma/settings.json", `{"project":{"id":"project-a"}}`)
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return hookrun.Env{Now: time.Now(), Cwd: root, Spool: sp, Version: "test", Policy: config.DefaultPolicy()}
}

func TestCodexStopCapturesRolloutAndDeduplicates(t *testing.T) {
	env := fundingEnv(t)
	id := "funding-session"
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "15", "rollout-day-"+id+".jsonl")
	at := env.Now.UTC().Format(time.RFC3339Nano)
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), `{"type":"session_meta","payload":{"id":"funding-session"}}`+"\n"+`{"timestamp":"`+at+`","type":"event_msg","payload":{"type":"token_count","rate_limits":{"plan_type":"team","credits":{"has_credits":false,"balance":"0"}}}}`+"\n")
	b, _ := json.Marshal(map[string]any{"session_id": id, "cwd": env.Cwd, "transcript_path": path})
	for range 2 {
		env.Stdin = strings.NewReader(string(b))
		_ = stop(context.Background(), env)
	}
	evs := hookruntest.Spooled(t, env.Spool)
	if len(evs) != 1 || evs[0].Name != hookrun.EventSessionQuota || evs[0].Attrs["plan_type"] != "team" || evs[0].Attrs["source_time"] != at || evs[0].Attrs["has_credits"] != false {
		t.Fatalf("delivered quota: %+v", evs)
	}
}

func TestCodexNotifyCapturesRolloutWithoutTranscriptPath(t *testing.T) {
	env := fundingEnv(t)
	id := "funding-session"
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "17", "rollout-day-"+id+".jsonl")
	at := env.Now.UTC().Format(time.RFC3339Nano)
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), `{"type":"session_meta","payload":{"id":"funding-session"}}`+"\n"+
		`{"timestamp":"`+at+`","type":"event_msg","payload":{"type":"token_count","rate_limits":{"plan_type":"team","primary":{"used_percent":42}}}}`+"\n")
	payload, _ := json.Marshal(map[string]any{
		"type": "agent-turn-complete", "thread-id": id, "turn-id": "turn-real", "cwd": env.Cwd, "model": "gpt-5.6-sol",
	})
	env.Args = []string{string(payload)}
	if err := notifyHook(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	evs := hookruntest.Spooled(t, env.Spool)
	var quota *spool.Event
	for i := range evs {
		if evs[i].Name == hookrun.EventSessionQuota {
			quota = &evs[i]
			break
		}
	}
	if quota == nil || quota.Attrs["plan_type"] != "team" || quota.Attrs["primary_used_pct"] != float64(42) {
		t.Fatalf("notify quota: %+v", evs)
	}
}

func TestCodexSequenceCheckpointAfterSpooling(t *testing.T) {
	env := fundingEnv(t)
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "rollout-day-funding-session.jsonl")
	data := `{"type":"session_meta","payload":{"id":"funding-session"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-a"}}` + "\n"
	for _, pct := range []string{"98", "100", "100"} {
		data += `{"timestamp":"2026-09-16T10:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"plan_type":"team","primary":{"used_percent":` + pct + `}}}}` + "\n"
	}
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), data)
	in := &codexHookInput{SessionID: "funding-session", TranscriptPath: path, Cwd: env.Cwd}
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// An unsuccessful append must leave quota observations available for retry.
	dir := filepath.Join(t.TempDir(), "spool")
	sp, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	env.Spool = sp
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	captureCodexFunding(env, context.Background(), r, in)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	captureCodexFunding(env, context.Background(), r, in)
	evs := hookruntest.Spooled(t, sp)
	if len(evs) != 3 {
		t.Fatalf("%+v", evs)
	}
	seen := map[any]bool{}
	for _, ev := range evs {
		id := ev.Attrs["observation_id"]
		if id == nil || seen[id] || ev.Attrs["turn_id"] != "turn-a" {
			t.Fatal(ev)
		}
		seen[id] = true
	}
	captureCodexFunding(env, context.Background(), r, in)
	if evs := hookruntest.Spooled(t, sp); len(evs) != 0 {
		t.Fatal(evs)
	}
}
