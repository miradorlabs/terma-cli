package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func writeRealClaudeAccount(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "claude_account_real.json"))
	if err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", string(b))
}

// A configured apiKeyHelper outranks the stored OAuth login, so the cached account is withheld.
func TestClaudeAccountWithheldWhenApiKeyHelperConfigured(t *testing.T) {
	env := newFundingEnv(t)
	writeRealClaudeAccount(t)

	if id, _, ok := claudeOAuthAccount(env.Cwd); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: claudeOAuthAccount = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}

	hookruntest.WriteFile(t, filepath.Join(env.Cwd, ".claude"), "settings.json", `{"apiKeyHelper":"/usr/local/bin/get-key"}`)
	if id, _, ok := claudeOAuthAccount(env.Cwd); ok {
		t.Fatalf("apiKeyHelper configured: expected the account withheld, got %q", id)
	}
}

// A non-regular settings.json makes the helper state "unknown", which withholds the account; a
// directory stands in for a symlink so the test runs on Windows.
func TestClaudeAccountWithheldWhenHelperStateUnknown(t *testing.T) {
	env := newFundingEnv(t)
	writeRealClaudeAccount(t)
	if id, _, ok := claudeOAuthAccount(env.Cwd); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: claudeOAuthAccount = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}
	if err := os.MkdirAll(filepath.Join(env.Cwd, ".claude", "settings.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if id, _, ok := claudeOAuthAccount(env.Cwd); ok {
		t.Fatalf("non-regular settings (helper state unknown): expected the account withheld, got %q", id)
	}
}

// A visible CLAUDE_CODE_OAUTH_TOKEN may belong to another account, so the cached id is withheld.
func TestClaudeAccountWithheldWhenOAuthTokenVisible(t *testing.T) {
	env := newFundingEnv(t)
	writeRealClaudeAccount(t)
	if id, _, ok := claudeOAuthAccount(env.Cwd); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: claudeOAuthAccount = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token-for-some-account")
	if id, _, ok := claudeOAuthAccount(env.Cwd); ok {
		t.Fatalf("visible oauth token: expected the account withheld, got %q", id)
	}
}

// A cloud route enabled with "yes" withholds the cached account too.
func TestClaudeAccountWithheldOnYesCloudFlag(t *testing.T) {
	env := newFundingEnv(t)
	writeRealClaudeAccount(t)
	if id, _, ok := claudeOAuthAccount(env.Cwd); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: claudeOAuthAccount = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "yes")
	if id, _, ok := claudeOAuthAccount(env.Cwd); ok {
		t.Fatalf("CLAUDE_CODE_USE_BEDROCK=yes: expected the account withheld, got %q", id)
	}
}

func TestClaudeAccountChangesAndDuplicateHooks(t *testing.T) {
	env := newFundingEnv(t)
	ctx := context.Background()
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-a","hasExtraUsageEnabled":false}}`)
	env.Stdin = strings.NewReader(hookPayload(env, "SessionStart"))
	_ = sessionStart(ctx, env)
	env.Stdin = strings.NewReader(hookPayload(env, "Stop"))
	_ = stop(ctx, env)
	evs := hookruntest.Spooled(t, env.Spool)
	if len(evs) != 2 || evs[1].Name != hookrun.EventSessionAccount || evs[1].Attrs[hookrun.AttrProjectID] != "project-a" {
		t.Fatalf("initial snapshot and duplicate stop: %+v", evs)
	}
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-b"}}`)
	env.Now = env.Now.Add(time.Second)
	env.Stdin = strings.NewReader(hookPayload(env, "Stop"))
	_ = stop(ctx, env)
	evs = hookruntest.Spooled(t, env.Spool)
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
			local.Stdin = strings.NewReader(hookPayload(local, "Stop"))
			_ = stop(ctx, local)
		})
	}
	wg.Wait()
	if evs = hookruntest.Spooled(t, env.Spool); len(evs) != 0 {
		t.Fatalf("duplicate snapshot: %+v", evs)
	}
	env.Now = env.Now.Add(hookrun.QuotaHeartbeat + time.Second)
	env.Stdin = strings.NewReader(hookPayload(env, "Stop"))
	_ = stop(ctx, env)
	if evs = hookruntest.Spooled(t, env.Spool); len(evs) != 1 {
		t.Fatalf("missing heartbeat: %+v", evs)
	}
}

func TestStopFailureAllowlist(t *testing.T) {
	env := newFundingEnv(t)
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-l"}}`)
	for _, kind := range []string{"rate_limit", "billing_error", "account_on_hold", "future-secret-type"} {
		b, _ := json.Marshal(map[string]any{"session_id": "funding-session", "cwd": env.Cwd, "error": kind, "error_details": "secret-details", "last_assistant_message": "secret-response"})
		env.Stdin = strings.NewReader(string(b))
		_ = stopFailure(context.Background(), env)
	}
	evs := hookruntest.Spooled(t, env.Spool)
	limits := 0
	for _, ev := range evs {
		if ev.Name == hookrun.EventSessionLimit {
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

// A failure under an override credential is not attributed to the cached OAuth account.
func TestStopFailureOmitsAccountIDUnderOverrideCredential(t *testing.T) {
	env := newFundingEnv(t)
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-l"}}`)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	b, _ := json.Marshal(map[string]any{"session_id": "funding-session", "cwd": env.Cwd, "error": "billing_error"})
	env.Stdin = strings.NewReader(string(b))
	_ = stopFailure(context.Background(), env)
	for _, ev := range hookruntest.Spooled(t, env.Spool) {
		if ev.Name != hookrun.EventSessionLimit {
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

func TestFundingRetriesAfterFailedAppend(t *testing.T) {
	env := newFundingEnv(t)
	dir := filepath.Join(t.TempDir(), "spool")
	sp, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	env.Spool = sp
	// Remove the spool directory so the first append fails even as root.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	env.Stdin = strings.NewReader(hookPayload(env, "Stop"))
	_ = stop(context.Background(), env)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	env.Stdin = strings.NewReader(hookPayload(env, "Stop"))
	_ = stop(context.Background(), env)
	if evs := hookruntest.Spooled(t, sp); len(evs) != 1 || evs[0].Name != hookrun.EventSessionAccount {
		t.Fatalf("failed append poisoned deduplication: %+v", evs)
	}
}

// realClaudeAccountID and realClaudeOrganizationID are the anonymized ids in
// testdata/claude_account_real.json, a real ~/.claude.json oauthAccount shape.
const (
	realClaudeAccountID      = "a1111111-1111-4111-8111-111111111111"
	realClaudeOrganizationID = "b2222222-2222-4222-8222-222222222222"
)
