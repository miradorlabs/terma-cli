package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func writeEvidenceFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeFundingSnapshotAndHints(t *testing.T) {
	dir, repo := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("ANTHROPIC_API_KEY", "never-export-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "never-export-token")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	writeEvidenceFile(t, filepath.Join(dir, ".claude.json"), `{"oauthAccount":{"accountUuid":"account-a","organizationUuid":"org-a","billingType":"stripe_subscription","organizationType":"claude_team","seatTier":"team_tier_1","hasExtraUsageEnabled":false,"cachedExtraUsageDisabledReason":"org_level_disabled_until","emailAddress":"private@example.test","accessToken":"never-export"},"projects":{"private":"never-export"}}`)
	writeEvidenceFile(t, filepath.Join(repo, ".claude", "settings.local.json"), `{"apiKeyHelper":"touch /tmp/never-run-this-command"}`)
	e := readFunding(repo)
	if e.Status != "present" || e.Attrs[semconv.TermaAccountIDKey] != "account-a" || e.Attrs[semconv.TermaAccountExtraUsageEnabledKey] != false || e.oauthEffective {
		t.Fatalf("snapshot: %+v", e)
	}
	// The credential hints decide the gate and never leave.
	for _, k := range []string{"api_key_helper_state", "api_key_present", "bedrock_enabled", "oauth_token_visibility", "hint_scope", "extra_usage_disabled_reason"} {
		if _, ok := e.Attrs[k]; ok {
			t.Fatalf("%s is spooled: %+v", k, e.Attrs)
		}
	}
	// Competing credentials keep account_id as raw evidence but withhold the cached login's email.
	if _, ok := e.Attrs[semconv.UserEmailKey]; ok {
		t.Fatalf("account_email attributed to a session using a different credential: %+v", e)
	}
	raw, _ := json.Marshal(e)
	for _, forbidden := range []string{"never-export", "never-run", "apiKeyHelper"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("captured forbidden content: %s", raw)
		}
	}
	// Missing fields remain absent, not false; an account switch is read afresh.
	writeEvidenceFile(t, filepath.Join(dir, ".claude.json"), `{"oauthAccount":{"accountUuid":"account-b","billingType":"prepaid"}}`)
	e = readFunding(repo)
	if e.Attrs[semconv.TermaAccountIDKey] != "account-b" {
		t.Fatal(e)
	}
	if _, ok := e.Attrs[semconv.TermaAccountExtraUsageEnabledKey]; ok {
		t.Fatal("unknown extra usage became false")
	}
}

// account_email is captured for a genuine OAuth session and withheld once a competing
// credential appears, the same gate as account_id attribution.
func TestClaudeFundingEmailGatedByEffectiveCredential(t *testing.T) {
	dir, repo := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		t.Setenv(k, "")
	}
	writeEvidenceFile(t, filepath.Join(dir, ".claude.json"), `{"oauthAccount":{"accountUuid":"account-a","emailAddress":"me@example.test"}}`)
	e := readFunding(repo)
	if !e.oauthEffective {
		t.Fatalf("a clean session's cached login is its credential: %+v", e)
	}
	if e.Attrs[semconv.UserEmailKey] != "me@example.test" {
		t.Fatalf("a genuine OAuth session keeps account_email: %+v", e)
	}
	t.Setenv("ANTHROPIC_API_KEY", "never-export-key")
	e = readFunding(repo)
	if _, ok := e.Attrs[semconv.UserEmailKey]; ok {
		t.Fatalf("account_email must be withheld under an env API key: %+v", e)
	}
	if e.Attrs[semconv.TermaAccountIDKey] != "account-a" {
		t.Fatalf("account_id stays as raw evidence: %+v", e)
	}
}

func TestClaudeFundingMissingMalformedAndSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, ".claude.json")
	for _, tc := range []struct{ data, status string }{
		{`{}`, "missing"}, {`{"oauthAccount":null}`, "missing"}, {`{"oauthAccount":[]}`, "malformed"}, {`{`, "malformed"}, {strings.Repeat("x", hookrun.EvidenceFileLimit+1), "oversized"},
	} {
		writeEvidenceFile(t, path, tc.data)
		if e := readFunding(""); e.Status != tc.status {
			t.Fatalf("want %s, got %s", tc.status, e.Status)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if e := readFunding(""); e.Status != "missing" {
		t.Fatal(e)
	}
	linked := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.Symlink(path, linked); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Dir(linked))
	if e := readFunding(""); e.Status != "unsupported" {
		t.Fatal(e)
	}
}
