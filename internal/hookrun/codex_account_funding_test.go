package hookrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/codex_rollout_real.jsonl are REAL rate-limit records (event_msg/token_count) captured from
// a live Codex rollout — rate_limits only, no conversation content — showing a team plan with the
// weekly (secondary) window at 91%. The auth.json shape is the real ~/.codex/auth.json (chatgpt route),
// account id anonymized. Together they pin that CodexStop stamps the ChatGPT account id onto the real
// funding evidence.
const realCodexAccountID = "a5c616f7-0e91-49d4-bcfb-000000000001"

func writeRealCodexAuth(t *testing.T, mode string, apiKey any) {
	t.Helper()
	doc := map[string]any{
		"OPENAI_API_KEY": apiKey,
		"auth_mode":      mode,
		"tokens": map[string]any{
			"id_token": "PLACEHOLDER", "access_token": "PLACEHOLDER", "refresh_token": "PLACEHOLDER",
			"account_id": realCodexAccountID,
		},
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func realRolloutTranscript(t *testing.T, env Env, sessionID string) string {
	t.Helper()
	lines, err := os.ReadFile(filepath.Join("testdata", "codex_rollout_real.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "16", "rollout-day-"+sessionID+".jsonl")
	content := `{"type":"session_meta","payload":{"id":"` + sessionID + `"}}` + "\n" + string(lines)
	writeFile(t, filepath.Dir(path), filepath.Base(path), content)
	return path
}

func TestCodexStopStampsRealChatGPTAccountID(t *testing.T) {
	env := fundingEnv(t)
	t.Setenv("OPENAI_API_KEY", "")
	writeRealCodexAuth(t, "chatgpt", nil)
	id := "funding-session"
	path := realRolloutTranscript(t, env, id)
	b, _ := json.Marshal(map[string]any{"session_id": id, "cwd": env.Cwd, "transcript_path": path})
	env.Stdin = strings.NewReader(string(b))
	if err := CodexStop(context.Background(), env); err != nil {
		t.Fatal(err)
	}

	evs := spooledQuota(t, env.Spool)
	if len(evs) == 0 {
		t.Fatal("no quota evidence spooled from the real rollout")
	}
	sawWeekly := false
	for _, ev := range evs {
		if ev.Attrs["account_id"] != realCodexAccountID {
			t.Fatalf("account_id = %v, want the ChatGPT account id %q", ev.Attrs["account_id"], realCodexAccountID)
		}
		if ev.Attrs["plan_type"] != "team" {
			t.Errorf("plan_type = %v, want team", ev.Attrs["plan_type"])
		}
		if pct, ok := ev.Attrs["secondary_used_pct"].(float64); ok && pct == 91 {
			sawWeekly = true
		}
	}
	if !sawWeekly {
		t.Error("expected a snapshot with the real weekly window at 91%")
	}
}

// A record with no present ChatGPT rate-limit evidence (rate_limits:null, as an --oss/custom-provider
// session emits) must not carry the account id even on the chatgpt route: that usage is not this
// account's. Mirrors the harness present-gate in captureCodexFunding.
func TestCodexStopOmitsAccountIDOnNullRateLimits(t *testing.T) {
	env := fundingEnv(t)
	t.Setenv("OPENAI_API_KEY", "")
	writeRealCodexAuth(t, "chatgpt", nil)
	id := "funding-session"
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "16", "rollout-day-"+id+".jsonl")
	content := `{"type":"session_meta","payload":{"id":"` + id + `"}}` + "\n" +
		`{"timestamp":"2026-09-16T08:20:00.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":null}}` + "\n"
	writeFile(t, filepath.Dir(path), filepath.Base(path), content)
	b, _ := json.Marshal(map[string]any{"session_id": id, "cwd": env.Cwd, "transcript_path": path})
	env.Stdin = strings.NewReader(string(b))
	if err := CodexStop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	evs := spooledQuota(t, env.Spool)
	sawUnavailable := false
	for _, ev := range evs {
		if ev.Attrs["evidence_status"] == "unavailable" {
			sawUnavailable = true
		}
		if _, ok := ev.Attrs["account_id"]; ok {
			t.Fatalf("account_id stamped on a non-present record: %+v", ev.Attrs)
		}
	}
	if !sawUnavailable {
		t.Error("expected an unavailable quota record from the null rate_limits snapshot")
	}
}

func TestCodexStopOmitsAccountIDOnAPIKeyRoute(t *testing.T) {
	env := fundingEnv(t)
	t.Setenv("OPENAI_API_KEY", "")
	writeRealCodexAuth(t, "apikey", "sk-key") // API-key route: cached OAuth account is not the payer
	id := "funding-session"
	path := realRolloutTranscript(t, env, id)
	b, _ := json.Marshal(map[string]any{"session_id": id, "cwd": env.Cwd, "transcript_path": path})
	env.Stdin = strings.NewReader(string(b))
	if err := CodexStop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	for _, ev := range spooledQuota(t, env.Spool) {
		if _, ok := ev.Attrs["account_id"]; ok {
			t.Fatalf("account_id stamped on API-key route: %+v", ev.Attrs)
		}
	}
}
