package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	e := ClaudeFunding(repo)
	if e.Status != "present" || e.Attrs["account_id"] != "account-a" || e.Attrs["extra_usage_enabled"] != false || e.Attrs["api_key_helper_state"] != "configured" || e.Attrs["api_key_present"] != true || e.Attrs["bedrock_enabled"] != true {
		t.Fatalf("snapshot: %+v", e)
	}
	if _, ok := e.Attrs["oauth_token_present"]; ok {
		t.Fatal("stripped OAuth variable treated as observed")
	}
	// This session stacks competing credentials (an env API key and auth token, Bedrock,
	// and a configured apiKeyHelper), so the cached OAuth login is NOT what it is using.
	// account_id stays as raw evidence beside those hints, but the email is the cached
	// login's and must be withheld — attributing it would tie that login to a session it
	// did not run (on a shared machine it could be a different person's email entirely).
	// A genuine OAuth session still gets it: see TestClaudeFundingEmailGatedByEffectiveCredential.
	if _, ok := e.Attrs["account_email"]; ok {
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
	e = ClaudeFunding(repo)
	if e.Attrs["account_id"] != "account-b" {
		t.Fatal(e)
	}
	if _, ok := e.Attrs["extra_usage_enabled"]; ok {
		t.Fatal("unknown extra usage became false")
	}
}

// account_email is captured for a genuine OAuth session (A1's keep-email intent) and
// withheld the moment a competing credential appears — the same gate that governs
// account_id attribution, so the cached login is never claimed for a session it did not
// run. account_id stays as raw evidence in both cases.
func TestClaudeFundingEmailGatedByEffectiveCredential(t *testing.T) {
	dir, repo := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		t.Setenv(k, "")
	}
	writeEvidenceFile(t, filepath.Join(dir, ".claude.json"), `{"oauthAccount":{"accountUuid":"account-a","emailAddress":"me@example.test"}}`)
	e := ClaudeFunding(repo)
	if e.Attrs["api_key_helper_state"] != "not_found" {
		t.Fatalf("a clean session has no apiKeyHelper: %+v", e)
	}
	if e.Attrs["account_email"] != "me@example.test" {
		t.Fatalf("a genuine OAuth session keeps account_email: %+v", e)
	}
	// A competing credential appears: the cached login is no longer the effective one.
	t.Setenv("ANTHROPIC_API_KEY", "never-export-key")
	e = ClaudeFunding(repo)
	if _, ok := e.Attrs["account_email"]; ok {
		t.Fatalf("account_email must be withheld under an env API key: %+v", e)
	}
	if e.Attrs["account_id"] != "account-a" {
		t.Fatalf("account_id stays as raw evidence: %+v", e)
	}
}

func TestClaudeFundingMissingMalformedAndSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, ".claude.json")
	for _, tc := range []struct{ data, status string }{
		{`{}`, "missing"}, {`{"oauthAccount":null}`, "missing"}, {`{"oauthAccount":[]}`, "malformed"}, {`{`, "malformed"}, {strings.Repeat("x", evidenceFileLimit+1), "oversized"},
	} {
		writeEvidenceFile(t, path, tc.data)
		if e := ClaudeFunding(""); e.Status != tc.status {
			t.Fatalf("want %s, got %s", tc.status, e.Status)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if e := ClaudeFunding(""); e.Status != "missing" {
		t.Fatal(e)
	}
	linked := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.Symlink(path, linked); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Dir(linked))
	if e := ClaudeFunding(""); e.Status != "unsupported" {
		t.Fatal(e)
	}
}

const testCodexID = "019947ab-1234-7000-8000-123456789abc"

func rolloutFixture(id string, at time.Time, limits string) string {
	return fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cli_version\":\"0.154.0\",\"cwd\":\"private-repo\"}}\n{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"total_tokens\":999},\"rate_limits\":%s}}\n", id, at.Format(time.RFC3339Nano), limits)
}

const testCodexLimits = `{"plan_type":"team","primary":{"used_percent":0,"window_minutes":300,"resets_at":1800000000},"secondary":null,"credits":{"has_credits":false,"unlimited":false,"balance":"12.345678901234567890"},"rate_limit_reached_type":"workspace_owner_credits_depleted","spend_control_reached":true,"secret":"never-export"}`

// latestCodexQuota reads a rollout the way the hooks do — ReadCodexFunding from an empty
// cursor, again while it reports a backlog — and returns the newest quota it emitted.
// When the rollout could not be opened at all it returns the reader's status instead,
// which is how the confinement refusals (a foreign path, a symlink, another thread's
// file) show up. These tests used to drive CodexFunding, a second tail reader nothing
// shipped called; they are the coverage of what a rollout read may touch and of what
// must never leave it, so they moved to the reader that runs.
func latestCodexQuota(t *testing.T, ctx context.Context, sessionID, transcript string) FundingEvidence {
	t.Helper()
	var last *FundingEvidence
	cursor, status := CodexCursor{}, ""
	for range 64 {
		var err error
		cursor, status, err = ReadCodexFunding(ctx, sessionID, transcript, cursor, func(e FundingEvidence) error {
			if e.Status != "gap" {
				last = &e
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read rollout: %v", err)
		}
		if status != "backlog" {
			break
		}
	}
	if last == nil {
		return FundingEvidence{Source: "codex_rollout", Status: status}
	}
	return *last
}

// quotaAttrs is an evidence's attributes without the reader's own bookkeeping.
func quotaAttrs(e FundingEvidence) map[string]any {
	out := map[string]any{}
	for k, v := range e.Attrs {
		switch k {
		case "source_offset", "source_stream", "observation_id", "turn_id":
		default:
			out[k] = v
		}
	}
	return out
}

func TestCodexFundingTailAndIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "sessions", "2026", "09", "15", "rollout-2026-09-15-"+testCodexID+".jsonl")
	at := time.Date(2026, 9, 15, 10, 0, 0, 123000000, time.UTC)
	writeEvidenceFile(t, path, rolloutFixture(testCodexID, at, testCodexLimits)+`{"unfinished":`)
	for _, hint := range []string{path, ""} {
		e := latestCodexQuota(t, context.Background(), testCodexID, hint)
		if e.Status != "present" || !e.SourceTime.Equal(at) || e.Attrs["has_credits"] != false || e.Attrs["credits_balance"] != "12.345678901234567890" || e.Attrs["primary_used_pct"] != 0.0 {
			t.Fatalf("snapshot: %+v", e)
		}
		if _, ok := e.Attrs["secondary_used_pct"]; ok {
			t.Fatal("absent secondary converted to zero")
		}
		raw, _ := json.Marshal(e)
		for _, s := range []string{"private-repo", "never-export", "total_tokens"} {
			if strings.Contains(string(raw), s) {
				t.Fatalf("content escaped: %s", raw)
			}
		}
	}
	writeEvidenceFile(t, path, rolloutFixture("other-thread", at, testCodexLimits))
	if e := latestCodexQuota(t, context.Background(), testCodexID, path); e.Status != "session_mismatch" {
		t.Fatal(e)
	}
	if e := latestCodexQuota(t, context.Background(), testCodexID, "/tmp/foreign.jsonl"); e.Status != "unsupported_path" {
		t.Fatal(e)
	}
}
func TestCodexFundingLargeRolloutNullAndMalformed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "archived_sessions", "rollout-2026-09-15-"+testCodexID+".jsonl")
	at := time.Now().UTC()
	original := rolloutFixture(testCodexID, at, testCodexLimits)
	_, record, _ := strings.Cut(original, "\n")
	// The reader must find metadata at the head and quota in a bounded tail.
	big := original + strings.Repeat("{\"type\":\"response_item\",\"text\":\"ignored\"}\n", 40000) + record
	writeEvidenceFile(t, path, big)
	if e := latestCodexQuota(t, context.Background(), testCodexID, path); e.Status != "present" {
		t.Fatal(e)
	}
	null := rolloutFixture(testCodexID, at.Add(time.Second), "null")
	_, nullRecord, _ := strings.Cut(null, "\n")
	writeEvidenceFile(t, path, original+nullRecord)
	if e := latestCodexQuota(t, context.Background(), testCodexID, path); e.Status != "unavailable" || len(quotaAttrs(e)) != 0 {
		t.Fatal(e)
	}
	writeEvidenceFile(t, path, rolloutFixture(testCodexID, at, `{"primary":{"used_percent":-1,"resets_at":-1},"credits":{"balance":"secret"}}`))
	e := latestCodexQuota(t, context.Background(), testCodexID, path)
	if _, ok := e.Attrs["primary_used_pct"]; ok {
		t.Fatal("bad percentage accepted")
	}
	if _, ok := e.Attrs["credits_balance"]; ok {
		t.Fatal("bad balance accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := latestCodexQuota(t, ctx, testCodexID, ""); e.Status != "search_limit" {
		t.Fatal(e)
	}
}

func TestCodexFundingRejectsSymlinksAndReportsUnreadableDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	foreign := filepath.Join(t.TempDir(), "rollout-day-"+testCodexID+".jsonl")
	writeEvidenceFile(t, foreign, rolloutFixture(testCodexID, time.Now(), testCodexLimits))
	if err := os.Mkdir(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "sessions", filepath.Base(foreign))
	if err := os.Symlink(foreign, link); err != nil {
		t.Fatal(err)
	}
	if e := latestCodexQuota(t, context.Background(), testCodexID, link); e.Status != "unsupported" || len(e.Attrs) != 0 {
		t.Fatal(e)
	}
	home = t.TempDir()
	t.Setenv("CODEX_HOME", home)
	writeEvidenceFile(t, filepath.Join(home, "sessions"), "not a directory")
	if e := latestCodexQuota(t, context.Background(), testCodexID, ""); e.Status != "unreadable" {
		t.Fatal(e)
	}
}
