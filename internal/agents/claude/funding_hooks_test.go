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

	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hookrun/hookruntest"
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

// A configured apiKeyHelper supplies an API credential that outranks the stored OAuth login, so the
// cached account is not the funding owner and must be withheld — like the env/cloud overrides.
func TestClaudeAccountWithheldWhenApiKeyHelperConfigured(t *testing.T) {
	env := newFundingEnv(t)
	writeRealClaudeAccount(t)

	// Control: with no apiKeyHelper, the OAuth account is the funding owner.
	if id, _, ok := claudeOAuthAccount(env.Cwd); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: claudeOAuthAccount = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}

	// A configured apiKeyHelper in repo settings now outranks the OAuth login.
	hookruntest.WriteFile(t, filepath.Join(env.Cwd, ".claude"), "settings.json", `{"apiKeyHelper":"/usr/local/bin/get-key"}`)
	if id, _, ok := claudeOAuthAccount(env.Cwd); ok {
		t.Fatalf("apiKeyHelper configured: expected the account withheld, got %q", id)
	}
}

// A settings.json that is not a regular file is rejected by the Lstat-based evidence reader as
// unsupported, so api_key_helper_state is "unknown" — a hidden apiKeyHelper cannot be ruled out, so the
// cached account is withheld. A symlinked settings.json (the dotfiles case that surfaced this) and a
// directory both hit the same non-regular -> unsupported path; a directory is used here so the test
// runs on every OS (Windows symlink creation needs elevated privileges).
func TestClaudeAccountWithheldWhenHelperStateUnknown(t *testing.T) {
	env := newFundingEnv(t)
	writeRealClaudeAccount(t)
	if id, _, ok := claudeOAuthAccount(env.Cwd); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: claudeOAuthAccount = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}
	// A directory at settings.json is a non-regular file: readEvidenceJSON reports unsupported, so the
	// helper state is "unknown" and the account is withheld.
	if err := os.MkdirAll(filepath.Join(env.Cwd, ".claude", "settings.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if id, _, ok := claudeOAuthAccount(env.Cwd); ok {
		t.Fatalf("non-regular settings (helper state unknown): expected the account withheld, got %q", id)
	}
}

// A VISIBLE CLAUDE_CODE_OAUTH_TOKEN is an externally supplied credential that may belong to a
// different account than the cached profile, so the cached id is withheld. (When Claude strips the
// token from the hook it is indistinguishable from interactive login and the cached profile stands.)
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

// Claude Code enables a cloud route on "yes" (its isOn accepts 1/true/yes), not only 1/true, so the
// cached OAuth account must be withheld there too — funding parses the flag with the same truthiness.
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

// The cached OAuth account must not be attributed to a failure that occurred
// under an override credential (env API key / auth token / cloud provider).
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
	// Remove the empty spool directory so the first append fails even as root.
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

// realClaudeAccountID and realClaudeOrganizationID are the account and organization UUIDs in testdata/claude_account_real.json — the real captured
// ~/.claude.json oauthAccount shape (Team plan, stripe_subscription billing), with the ids anonymized
// to match the platform's real_claude_account.json fixture. writeRealClaudeAccount lands it as the
// hook's .claude.json so the funding tests run against the true wire shape, not a stub.
const (
	realClaudeAccountID      = "a1111111-1111-4111-8111-111111111111"
	realClaudeOrganizationID = "b2222222-2222-4222-8222-222222222222"
)
