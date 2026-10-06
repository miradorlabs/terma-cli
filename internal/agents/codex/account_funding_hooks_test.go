package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// testdata/codex_rollout_real.jsonl holds real rate-limit records only (a team plan, weekly
// window at 91%); with a real auth.json shape they pin that stop stamps the account id.
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

func realRolloutTranscript(t *testing.T, env hookrun.Env, sessionID string) string {
	t.Helper()
	lines, err := os.ReadFile(filepath.Join("testdata", "codex_rollout_real.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "16", "rollout-day-"+sessionID+".jsonl")
	content := `{"type":"session_meta","payload":{"id":"` + sessionID + `"}}` + "\n" + turnContext("turn-real", env.Cwd) + string(lines)
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), content)
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
	if err := stop(context.Background(), env); err != nil {
		t.Fatal(err)
	}

	evs := hookruntest.Spooled(t, env.Spool)
	if len(evs) == 0 {
		t.Fatal("no quota evidence spooled from the real rollout")
	}
	sawWeekly := false
	for _, ev := range evs {
		if ev.Attrs[semconv.TermaAccountIDKey] != realCodexAccountID {
			t.Fatalf("account_id = %v, want the ChatGPT account id %q", ev.Attrs[semconv.TermaAccountIDKey], realCodexAccountID)
		}
		if ev.Attrs[semconv.TermaAccountPlanTypeKey] != "team" {
			t.Errorf("plan_type = %v, want team", ev.Attrs[semconv.TermaAccountPlanTypeKey])
		}
		if pct, ok := ev.Attrs[semconv.TermaRateLimitSecondaryUsedPercentKey].(float64); ok && pct == 91 {
			sawWeekly = true
		}
	}
	if !sawWeekly {
		t.Error("expected a snapshot with the real weekly window at 91%")
	}
}

// A record without present ChatGPT rate-limit evidence never carries the account id, even
// on the chatgpt route.
func TestCodexStopOmitsAccountIDOnNullRateLimits(t *testing.T) {
	env := fundingEnv(t)
	t.Setenv("OPENAI_API_KEY", "")
	writeRealCodexAuth(t, "chatgpt", nil)
	id := "funding-session"
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "16", "rollout-day-"+id+".jsonl")
	content := `{"type":"session_meta","payload":{"id":"` + id + `"}}` + "\n" + turnContext("turn-real", env.Cwd) +
		`{"timestamp":"2026-09-16T08:20:00.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":null}}` + "\n"
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), content)
	b, _ := json.Marshal(map[string]any{"session_id": id, "cwd": env.Cwd, "transcript_path": path})
	env.Stdin = strings.NewReader(string(b))
	if err := stop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	evs := hookruntest.Spooled(t, env.Spool)
	sawUnavailable := false
	for _, ev := range evs {
		if ev.Attrs[semconv.TermaEvidenceStatusKey] == "unavailable" {
			sawUnavailable = true
		}
		if _, ok := ev.Attrs[semconv.TermaAccountIDKey]; ok {
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
	writeRealCodexAuth(t, "apikey", "sk-key")
	id := "funding-session"
	path := realRolloutTranscript(t, env, id)
	b, _ := json.Marshal(map[string]any{"session_id": id, "cwd": env.Cwd, "transcript_path": path})
	env.Stdin = strings.NewReader(string(b))
	if err := stop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	for _, ev := range hookruntest.Spooled(t, env.Spool) {
		if _, ok := ev.Attrs[semconv.TermaAccountIDKey]; ok {
			t.Fatalf("account_id stamped on API-key route: %+v", ev.Attrs)
		}
	}
}

// A seat in a shared workspace is the login's email beside the account; the seat id stands
// in for the email wherever the team's policy withholds it, as the relay's does.
func TestCodexQuotaNamesTheSeatAndWithholdsTheEmailWithContent(t *testing.T) {
	env := fundingEnv(t)
	t.Setenv("OPENAI_API_KEY", "")
	writeRealCodexAuth(t, "chatgpt", nil)
	claims, _ := json.Marshal(map[string]any{"email": "dev@example.com", "email_verified": true, "https://api.openai.com/auth": map[string]any{"user_id": "user-1"}})
	authPath := filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")
	auth, _ := os.ReadFile(authPath)
	auth = []byte(strings.Replace(string(auth), `"id_token": "PLACEHOLDER"`, `"id_token": "e30.`+base64.RawURLEncoding.EncodeToString(claims)+`.sig"`, 1))
	if err := os.WriteFile(authPath, auth, 0o600); err != nil {
		t.Fatal(err)
	}
	id := "funding-session"
	path := realRolloutTranscript(t, env, id)
	b, _ := json.Marshal(map[string]any{"session_id": id, "cwd": env.Cwd, "transcript_path": path})
	env.Stdin = strings.NewReader(string(b))
	if err := stop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	quotas := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSessionQuotaEvent)
	if len(quotas) == 0 {
		t.Fatal("no quota spooled")
	}
	seat := shape.SeatID(realCodexAccountID, "dev@example.com")
	for _, collects := range []bool{true, false} {
		env.Policy.IncludePrompts, env.Policy.IncludeToolContent = collects, collects
		for _, q := range delivered(env, quotas) {
			if q.Attrs[semconv.TermaAccountSeatIDKey] != seat {
				t.Fatalf("seat id = %v, want %s", q.Attrs[semconv.TermaAccountSeatIDKey], seat)
			}
			if email, has := q.Attrs[semconv.UserEmailKey]; has != collects || has && email != "dev@example.com" {
				t.Fatalf("content collected %v: user.email = %v", collects, email)
			}
			if _, ok := q.Attrs["account_user_id"]; ok {
				t.Fatalf("the login's user id is sent: %+v", q.Attrs)
			}
		}
	}
}
