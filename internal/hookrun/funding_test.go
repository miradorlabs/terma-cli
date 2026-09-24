package hookrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/spool"
)

func fundingEnv(t *testing.T) Env {
	t.Helper()
	root := initRepo(t)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	writeFile(t, root, ".terma/settings.json", `{"project":{"id":"project-a"}}`)
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Env{Now: time.Now(), Cwd: root, Spool: sp, Version: "test"}
}
func hookInput(env Env, event string) string {
	b, _ := json.Marshal(map[string]any{"session_id": "funding-session", "cwd": env.Cwd, "hook_event_name": event})
	return string(b)
}

// realClaudeAccountID is the account UUID in testdata/claude_account_real.json — the real captured
// ~/.claude.json oauthAccount shape (Team plan, stripe_subscription billing), with the ids anonymized
// to match the platform's real_claude_account.json fixture. writeRealClaudeAccount lands it as the
// hook's .claude.json so the funding tests run against the true wire shape, not a stub.
const realClaudeAccountID = "a1111111-1111-4111-8111-111111111111"

func writeRealClaudeAccount(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "claude_account_real.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", string(b))
}

// A configured apiKeyHelper supplies an API credential that outranks the stored OAuth login, so the
// cached account is not the funding owner and must be withheld — like the env/cloud overrides.
func TestClaudeAccountWithheldWhenApiKeyHelperConfigured(t *testing.T) {
	env := fundingEnv(t)
	writeRealClaudeAccount(t)

	// Control: with no apiKeyHelper, the OAuth account is the funding owner.
	if id, ok := claudeOAuthAccountID(env.Cwd); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: claudeOAuthAccountID = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}

	// A configured apiKeyHelper in repo settings now outranks the OAuth login.
	writeFile(t, filepath.Join(env.Cwd, ".claude"), "settings.json", `{"apiKeyHelper":"/usr/local/bin/get-key"}`)
	if id, ok := claudeOAuthAccountID(env.Cwd); ok {
		t.Fatalf("apiKeyHelper configured: expected the account withheld, got %q", id)
	}
}

// A settings.json that is not a regular file is rejected by the Lstat-based evidence reader as
// unsupported, so api_key_helper_state is "unknown" — a hidden apiKeyHelper cannot be ruled out, so the
// cached account is withheld. A symlinked settings.json (the dotfiles case that surfaced this) and a
// directory both hit the same non-regular -> unsupported path; a directory is used here so the test
// runs on every OS (Windows symlink creation needs elevated privileges).
func TestClaudeAccountWithheldWhenHelperStateUnknown(t *testing.T) {
	env := fundingEnv(t)
	writeRealClaudeAccount(t)
	if id, ok := claudeOAuthAccountID(env.Cwd); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: claudeOAuthAccountID = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}
	// A directory at settings.json is a non-regular file: readEvidenceJSON reports unsupported, so the
	// helper state is "unknown" and the account is withheld.
	if err := os.MkdirAll(filepath.Join(env.Cwd, ".claude", "settings.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if id, ok := claudeOAuthAccountID(env.Cwd); ok {
		t.Fatalf("non-regular settings (helper state unknown): expected the account withheld, got %q", id)
	}
}

// A VISIBLE CLAUDE_CODE_OAUTH_TOKEN is an externally supplied credential that may belong to a
// different account than the cached profile, so the cached id is withheld. (When Claude strips the
// token from the hook it is indistinguishable from interactive login and the cached profile stands.)
func TestClaudeAccountWithheldWhenOAuthTokenVisible(t *testing.T) {
	env := fundingEnv(t)
	writeRealClaudeAccount(t)
	if id, ok := claudeOAuthAccountID(env.Cwd); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: claudeOAuthAccountID = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token-for-some-account")
	if id, ok := claudeOAuthAccountID(env.Cwd); ok {
		t.Fatalf("visible oauth token: expected the account withheld, got %q", id)
	}
}

// Claude Code enables a cloud route on "yes" (its isOn accepts 1/true/yes), not only 1/true, so the
// cached OAuth account must be withheld there too — funding parses the flag with the same truthiness.
func TestClaudeAccountWithheldOnYesCloudFlag(t *testing.T) {
	env := fundingEnv(t)
	writeRealClaudeAccount(t)
	if id, ok := claudeOAuthAccountID(env.Cwd); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: claudeOAuthAccountID = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "yes")
	if id, ok := claudeOAuthAccountID(env.Cwd); ok {
		t.Fatalf("CLAUDE_CODE_USE_BEDROCK=yes: expected the account withheld, got %q", id)
	}
}
func TestClaudeAccountChangesAndDuplicateHooks(t *testing.T) {
	env := fundingEnv(t)
	ctx := context.Background()
	writeFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-a","hasExtraUsageEnabled":false}}`)
	env.Stdin = strings.NewReader(hookInput(env, "SessionStart"))
	_ = SessionStart(ctx, env)
	env.Stdin = strings.NewReader(hookInput(env, "Stop"))
	_ = Stop(ctx, env)
	evs := spooledQuota(t, env.Spool)
	if len(evs) != 2 || evs[1].Name != EventSessionAccount || evs[1].Attrs[AttrProjectID] != "project-a" {
		t.Fatalf("initial snapshot and duplicate stop: %+v", evs)
	}
	writeFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-b"}}`)
	env.Now = env.Now.Add(time.Second)
	env.Stdin = strings.NewReader(hookInput(env, "Stop"))
	_ = Stop(ctx, env)
	evs = spooledQuota(t, env.Spool)
	if len(evs) != 1 || evs[0].Attrs["account_id"] != "account-b" {
		t.Fatalf("account switch: %+v", evs)
	}
	if _, ok := evs[0].Attrs["extra_usage_enabled"]; ok {
		t.Fatal("missing policy became false")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			local := env
			local.Stdin = strings.NewReader(hookInput(local, "Stop"))
			_ = Stop(ctx, local)
		})
	}
	wg.Wait()
	if evs = spooledQuota(t, env.Spool); len(evs) != 0 {
		t.Fatalf("duplicate snapshot: %+v", evs)
	}
	env.Now = env.Now.Add(quotaHeartbeat + time.Second)
	env.Stdin = strings.NewReader(hookInput(env, "Stop"))
	_ = Stop(ctx, env)
	if evs = spooledQuota(t, env.Spool); len(evs) != 1 {
		t.Fatalf("missing heartbeat: %+v", evs)
	}
}
func TestStopFailureAllowlist(t *testing.T) {
	env := fundingEnv(t)
	writeFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-l"}}`)
	for _, kind := range []string{"rate_limit", "billing_error", "account_on_hold", "future-secret-type"} {
		b, _ := json.Marshal(map[string]any{"session_id": "funding-session", "cwd": env.Cwd, "error": kind, "error_details": "secret-details", "last_assistant_message": "secret-response"})
		env.Stdin = strings.NewReader(string(b))
		_ = StopFailure(context.Background(), env)
	}
	evs := spooledQuota(t, env.Spool)
	limits := 0
	for _, ev := range evs {
		if ev.Name == EventSessionLimit {
			limits++
			if ev.Attrs["account_id"] != "account-l" || ev.Attrs["schema_version"] != float64(1) {
				t.Fatalf("limit record missing account identity: %+v", ev)
			}
			if limits == 4 && ev.Attrs["error_type"] != "unknown" {
				t.Fatal(ev)
			}
		}
	}
	if limits != 4 {
		t.Fatalf("failure events: %+v", evs)
	}
	raw, _ := json.Marshal(evs)
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("private details escaped: %s", raw)
	}
}

// The cached OAuth account must not be attributed to a failure that occurred
// under an override credential (env API key / auth token / cloud provider).
func TestStopFailureOmitsAccountIDUnderOverrideCredential(t *testing.T) {
	env := fundingEnv(t)
	writeFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-l"}}`)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	b, _ := json.Marshal(map[string]any{"session_id": "funding-session", "cwd": env.Cwd, "error": "billing_error"})
	env.Stdin = strings.NewReader(string(b))
	_ = StopFailure(context.Background(), env)
	for _, ev := range spooledQuota(t, env.Spool) {
		if ev.Name != EventSessionLimit {
			continue
		}
		if _, ok := ev.Attrs["account_id"]; ok {
			t.Fatalf("account_id stamped on limit under override credential: %+v", ev.Attrs)
		}
		if ev.Attrs["schema_version"] != float64(1) {
			t.Fatalf("schema_version: %+v", ev.Attrs)
		}
	}
}

func TestCodexStopCapturesRolloutAndDeduplicates(t *testing.T) {
	env := fundingEnv(t)
	id := "funding-session"
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "15", "rollout-day-"+id+".jsonl")
	at := env.Now.UTC().Format(time.RFC3339Nano)
	writeFile(t, filepath.Dir(path), filepath.Base(path), `{"type":"session_meta","payload":{"id":"funding-session"}}`+"\n"+`{"timestamp":"`+at+`","type":"event_msg","payload":{"type":"token_count","rate_limits":{"plan_type":"team","credits":{"has_credits":false,"balance":"0"}}}}`+"\n")
	b, _ := json.Marshal(map[string]any{"session_id": id, "cwd": env.Cwd, "transcript_path": path})
	for range 2 {
		env.Stdin = strings.NewReader(string(b))
		_ = CodexStop(context.Background(), env)
	}
	evs := spooledQuota(t, env.Spool)
	if len(evs) != 1 || evs[0].Name != EventSessionQuota || evs[0].Attrs["plan_type"] != "team" || evs[0].Attrs["source_time"] != at || evs[0].Attrs["has_credits"] != false {
		t.Fatalf("delivered quota: %+v", evs)
	}
}

func TestCodexNotifyCapturesRolloutWithoutTranscriptPath(t *testing.T) {
	env := fundingEnv(t)
	id := "funding-session"
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "17", "rollout-day-"+id+".jsonl")
	at := env.Now.UTC().Format(time.RFC3339Nano)
	writeFile(t, filepath.Dir(path), filepath.Base(path), `{"type":"session_meta","payload":{"id":"funding-session"}}`+"\n"+
		`{"timestamp":"`+at+`","type":"event_msg","payload":{"type":"token_count","rate_limits":{"plan_type":"team","primary":{"used_percent":42}}}}`+"\n")
	payload, _ := json.Marshal(map[string]any{
		"type": "agent-turn-complete", "thread-id": id, "turn-id": "turn-real", "cwd": env.Cwd, "model": "gpt-5.6-sol",
	})
	env.Args = []string{string(payload)}
	if err := CodexNotify(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	evs := spooledQuota(t, env.Spool)
	var quota *spool.Event
	for i := range evs {
		if evs[i].Name == EventSessionQuota {
			quota = &evs[i]
			break
		}
	}
	if quota == nil || quota.Attrs["plan_type"] != "team" || quota.Attrs["primary_used_pct"] != float64(42) {
		t.Fatalf("notify quota: %+v", evs)
	}
}

func TestFundingRetriesAfterFailedAppend(t *testing.T) {
	env := fundingEnv(t)
	dir := filepath.Join(t.TempDir(), "spool")
	sp, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	env.Spool = sp
	// Remove the empty spool directory so the first append fails even as root.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	env.Stdin = strings.NewReader(hookInput(env, "Stop"))
	_ = Stop(context.Background(), env)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	env.Stdin = strings.NewReader(hookInput(env, "Stop"))
	_ = Stop(context.Background(), env)
	if evs := spooledQuota(t, sp); len(evs) != 1 || evs[0].Name != EventSessionAccount {
		t.Fatalf("failed append poisoned deduplication: %+v", evs)
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
	writeFile(t, filepath.Dir(path), filepath.Base(path), data)
	in := &codexHookInput{SessionID: "funding-session", TranscriptPath: path, Cwd: env.Cwd}
	r, err := env.repo(context.Background())
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
	env.captureCodexFunding(context.Background(), r, in)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	env.captureCodexFunding(context.Background(), r, in)
	evs := spooledQuota(t, sp)
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
	env.captureCodexFunding(context.Background(), r, in)
	if evs := spooledQuota(t, sp); len(evs) != 0 {
		t.Fatal(evs)
	}
}
