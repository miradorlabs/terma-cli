package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
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

	if id, _, ok := readFunding(env.Cwd).oauthAccount(); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: oauthAccount = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}

	hookruntest.WriteFile(t, filepath.Join(env.Cwd, ".claude"), "settings.json", `{"apiKeyHelper":"/usr/local/bin/get-key"}`)
	if id, _, ok := readFunding(env.Cwd).oauthAccount(); ok {
		t.Fatalf("apiKeyHelper configured: expected the account withheld, got %q", id)
	}
}

// A non-regular settings.json makes the helper state "unknown", which withholds the account; a
// directory stands in for a symlink so the test runs on Windows.
func TestClaudeAccountWithheldWhenHelperStateUnknown(t *testing.T) {
	env := newFundingEnv(t)
	writeRealClaudeAccount(t)
	if id, _, ok := readFunding(env.Cwd).oauthAccount(); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: oauthAccount = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}
	if err := os.MkdirAll(filepath.Join(env.Cwd, ".claude", "settings.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if id, _, ok := readFunding(env.Cwd).oauthAccount(); ok {
		t.Fatalf("non-regular settings (helper state unknown): expected the account withheld, got %q", id)
	}
}

// A visible CLAUDE_CODE_OAUTH_TOKEN may belong to another account, so the cached id is withheld.
func TestClaudeAccountWithheldWhenOAuthTokenVisible(t *testing.T) {
	env := newFundingEnv(t)
	writeRealClaudeAccount(t)
	if id, _, ok := readFunding(env.Cwd).oauthAccount(); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: oauthAccount = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token-for-some-account")
	if id, _, ok := readFunding(env.Cwd).oauthAccount(); ok {
		t.Fatalf("visible oauth token: expected the account withheld, got %q", id)
	}
}

// A cloud route enabled with "yes" withholds the cached account too.
func TestClaudeAccountWithheldOnYesCloudFlag(t *testing.T) {
	env := newFundingEnv(t)
	writeRealClaudeAccount(t)
	if id, _, ok := readFunding(env.Cwd).oauthAccount(); !ok || id != realClaudeAccountID {
		t.Fatalf("baseline: oauthAccount = %q,%v; want %q,true", id, ok, realClaudeAccountID)
	}
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "yes")
	if id, _, ok := readFunding(env.Cwd).oauthAccount(); ok {
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
	if len(evs) != 2 || evs[1].Name != semconv.TermaSessionAccountEvent || evs[1].Attrs[hookrun.AttrProjectID] != "project-a" {
		t.Fatalf("initial snapshot and duplicate stop: %+v", evs)
	}
	// Only the session's own /login moves it to the stored login.
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-b"}}`)
	env.Now = env.Now.Add(time.Second)
	transcript := filepath.Join(t.TempDir(), "funding-session.jsonl")
	stopIn, _ := json.Marshal(map[string]any{"session_id": "funding-session", "cwd": env.Cwd, "hook_event_name": "Stop", "transcript_path": transcript})
	env.Stdin = strings.NewReader(string(stopIn))
	_ = stop(ctx, env)
	if evs = hookruntest.Spooled(t, env.Spool); len(evs) != 0 {
		t.Fatalf("another session's login relabelled this one: %+v", evs)
	}
	appendLogin(t, transcript, "funding-session", env.Now)
	env.Stdin = strings.NewReader(string(stopIn))
	_ = stop(ctx, env)
	evs = hookruntest.Spooled(t, env.Spool)
	if len(evs) != 1 || evs[0].Attrs[semconv.TermaAccountIDKey] != "account-b" {
		t.Fatalf("account switch: %+v", evs)
	}
	if _, ok := evs[0].Attrs[semconv.TermaAccountExtraUsageEnabledKey]; ok {
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
		if ev.Name == semconv.TermaSessionLimitEvent {
			limits++
			if ev.Attrs[semconv.TermaAccountIDKey] != "account-l" {
				t.Fatalf("limit record missing account identity: %+v", ev)
			}
			if limits == 4 && ev.Attrs[semconv.ErrorTypeKey] != semconv.ErrorTypeOther {
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

// A limit hit after another session's /login is the session's own account's.
func TestStopFailureKeepsTheSessionsAccount(t *testing.T) {
	env := newFundingEnv(t)
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-l"}}`)
	env.Stdin = strings.NewReader(hookPayload(env, "SessionStart"))
	_ = sessionStart(context.Background(), env)
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-m"}}`)
	b, _ := json.Marshal(map[string]any{"session_id": "funding-session", "cwd": env.Cwd, "error": "rate_limit"})
	env.Stdin = strings.NewReader(string(b))
	_ = stopFailure(context.Background(), env)
	limits := 0
	for _, ev := range hookruntest.Spooled(t, env.Spool) {
		if ev.Name == semconv.TermaSessionLimitEvent {
			limits++
			if ev.Attrs[semconv.TermaAccountIDKey] != "account-l" {
				t.Fatalf("limit record relabelled: %+v", ev.Attrs)
			}
		}
	}
	if limits != 1 {
		t.Fatalf("limit records: %d", limits)
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
		if ev.Name != semconv.TermaSessionLimitEvent {
			continue
		}
		if _, ok := ev.Attrs[semconv.TermaAccountIDKey]; ok {
			t.Fatalf("account_id stamped on limit under override credential: %+v", ev.Attrs)
		}
		if ev.Attrs[semconv.ErrorTypeKey] != "billing_error" {
			t.Fatalf("error.type: %+v", ev.Attrs)
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
	if evs := hookruntest.Spooled(t, sp); len(evs) != 1 || evs[0].Name != semconv.TermaSessionAccountEvent {
		t.Fatalf("failed append poisoned deduplication: %+v", evs)
	}
}

// A new process reads the stored login afresh; a compacted session keeps the one it had.
func TestClaudeAccountRepinnedOnlyByNewProcess(t *testing.T) {
	env := newFundingEnv(t)
	start := func(source string) map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"session_id": "funding-session", "cwd": env.Cwd, "hook_event_name": "SessionStart", "source": source})
		env.Stdin = strings.NewReader(string(b))
		env.Now = env.Now.Add(hookrun.QuotaHeartbeat + time.Second)
		_ = sessionStart(context.Background(), env)
		for _, ev := range hookruntest.Spooled(t, env.Spool) {
			if ev.Name == semconv.TermaSessionAccountEvent {
				return ev.Attrs
			}
		}
		t.Fatalf("%s: no account event", source)
		return nil
	}
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-a","emailAddress":"dev@example.com"}}`)
	start("startup")
	pins, _ := filepath.Glob(filepath.Join(env.StateDir, claudeAccountStateDir, "*.json"))
	for _, p := range pins {
		if b, _ := os.ReadFile(p); strings.Contains(string(b), "example.com") {
			t.Fatalf("the email reached state: %s", b)
		}
	}
	if len(pins) != 1 {
		t.Fatalf("pins: %v", pins)
	}
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-b"}}`)
	if got := start("compact")[semconv.TermaAccountIDKey]; got != "account-a" {
		t.Fatalf("compact: account %v, want account-a", got)
	}
	if got := start("resume")[semconv.TermaAccountIDKey]; got != "account-b" {
		t.Fatalf("resume: account %v, want account-b", got)
	}
}

// A /login that kept the account is retired, so a later login elsewhere leaves the session alone.
func TestClaudeAccountSameAccountLoginRetired(t *testing.T) {
	env := newFundingEnv(t)
	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-a"}}`)
	sessionFunding(env, env.Cwd, "s", transcript, true)
	appendLogin(t, transcript, "s", env.Now.Add(time.Second))
	env.Now = env.Now.Add(2 * time.Minute)
	sessionFunding(env, env.Cwd, "s", transcript, false)
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-b"}}`)
	env.Now = env.Now.Add(time.Minute)
	if id, _ := sessionFunding(env, env.Cwd, "s", transcript, false).accountID(); id != "account-a" {
		t.Fatalf("account %q, want account-a", id)
	}
}

// A hook that finds the pin's lock taken still holds the session's account, and a held pin is
// kept fresh for the sweep.
func TestClaudeAccountHeldUnderContentionAndSweep(t *testing.T) {
	env := newFundingEnv(t)
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-a"}}`)
	sessionFunding(env, env.Cwd, "s", "", true)
	hookruntest.WriteFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json", `{"oauthAccount":{"accountUuid":"account-b"}}`)
	dir := filepath.Join(env.StateDir, claudeAccountStateDir)
	path := filepath.Join(dir, hookrun.EvidenceID("s")+".json")
	stale := env.Now.Add(-spool.MaxAge - time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := sessionFunding(env, env.Cwd, "s", "", false).accountID()
	unlock()
	if id != "account-a" {
		t.Fatalf("under a busy lock: account %q, want account-a", id)
	}
	hookrun.PruneState(dir, env.Now.Add(-spool.MaxAge))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a held pin was swept: %v", err)
	}
	// Within the hour a held redraw writes nothing.
	recent := env.Now.Add(-30 * time.Minute).Truncate(time.Second)
	if err := os.Chtimes(path, recent, recent); err != nil {
		t.Fatal(err)
	}
	sessionFunding(env, env.Cwd, "s", "", false)
	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(recent) {
		t.Fatalf("a held pin was touched within the hour: %v %v", info.ModTime(), err)
	}
}

// Only a /login the developer typed in this session, after the pin, counts.
func TestLoggedInSince(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	own := filepath.Join(dir, "own.jsonl")
	appendLogin(t, own, "s", since.Add(time.Second))
	earlier := filepath.Join(dir, "earlier.jsonl")
	appendLogin(t, earlier, "s", since.Add(-time.Second))
	other := filepath.Join(dir, "other.jsonl")
	appendLogin(t, other, "another-session", since.Add(time.Second))
	quoted := filepath.Join(dir, "quoted.jsonl")
	hookruntest.WriteFile(t, dir, "quoted.jsonl", `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"<command-name>/login</command-name>"}]},"timestamp":"2026-10-04T10:00:01Z","sessionId":"s"}`+"\n")
	for _, tc := range []struct {
		path string
		want bool
	}{{own, true}, {earlier, false}, {other, false}, {quoted, false}, {filepath.Join(dir, "missing.jsonl"), false}, {"", false}} {
		if got := loggedInSince(tc.path, "s", since); got != tc.want {
			t.Errorf("%s: loggedInSince = %v, want %v", filepath.Base(tc.path), got, tc.want)
		}
	}
}

// realClaudeAccountID and realClaudeOrganizationID are the anonymized ids in
// testdata/claude_account_real.json, a real ~/.claude.json oauthAccount shape.
const (
	realClaudeAccountID      = "a1111111-1111-4111-8111-111111111111"
	realClaudeOrganizationID = "b2222222-2222-4222-8222-222222222222"
)

// limitRun runs hooks for the session over a transcript and returns the limits spooled.
func limitRun(t *testing.T, env hookrun.Env, transcript, kind string, hooks ...func(context.Context, hookrun.Env) error) []spool.Event {
	t.Helper()
	for _, hook := range hooks {
		b, _ := json.Marshal(map[string]any{"session_id": "funding-session", "cwd": env.Cwd, "transcript_path": transcript, "error": kind})
		env.Stdin = strings.NewReader(string(b))
		_ = hook(context.Background(), env)
	}
	var limits []spool.Event
	for _, ev := range hookruntest.Spooled(t, env.Spool) {
		if ev.Name == semconv.TermaSessionLimitEvent {
			limits = append(limits, ev)
		}
	}
	return limits
}

// A headless run's StopFailure can die before it reports; the session's end reports the
// transcript's API error once, and never one StopFailure already reported.
func TestSessionEndReportsAnUnreportedLimit(t *testing.T) {
	apiError := func(at time.Time, kind string) string {
		b, _ := json.Marshal(map[string]any{"type": "assistant", "sessionId": "funding-session", "timestamp": at,
			"isApiErrorMessage": true, "apiErrorStatus": 429, "error": kind, "message": map[string]any{"content": "secret"}})
		return string(b)
	}
	env := newFundingEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "funding-session.jsonl")
	write := func(lines ...string) {
		hookruntest.WriteFile(t, dir, "funding-session.jsonl", strings.Join(lines, "\n")+"\n")
	}

	write(`{"type":"user","sessionId":"funding-session","message":{"content":"hi"}}`,
		`{"type":"assistant","sessionId":"another-session","timestamp":"2026-01-01T00:00:00Z","isApiErrorMessage":true,"error":"rate_limit"}`)
	if got := limitRun(t, env, path, "", sessionEnd); len(got) != 0 {
		t.Fatalf("no API error of the session's, yet %+v", got)
	}

	// A session resumed after the sweep may have lost its marker.
	write(apiError(env.Now.Add(-spool.MaxAge-time.Second), "overloaded"))
	if got := limitRun(t, env, path, "", sessionEnd); len(got) != 0 {
		t.Fatalf("an error past the marker's retention was reported: %+v", got)
	}

	write(apiError(env.Now.Add(-time.Second), "overloaded"))
	got := limitRun(t, env, path, "", sessionEnd)
	if len(got) != 1 || got[0].Attrs[semconv.ErrorTypeKey] != "overloaded" || got[0].Attrs[semconv.TermaEvidenceSourceKey] != sourceClaudeTranscript {
		t.Fatalf("unreported limit: %+v", got)
	}
	if attrs := fmt.Sprint(got[0].Attrs); strings.Contains(attrs, "secret") {
		t.Fatalf("transcript detail escaped: %s", attrs)
	}
	if got := limitRun(t, env, path, "", sessionEnd); len(got) != 0 {
		t.Fatalf("a resumed session's end reported the limit again: %+v", got)
	}

	env.Now = env.Now.Add(time.Minute)
	write(apiError(env.Now.Add(-time.Second), "overloaded"))
	got = limitRun(t, env, path, "rate_limit", stopFailure, sessionEnd)
	if len(got) != 1 || got[0].Attrs[semconv.TermaEvidenceSourceKey] != sourceClaudeStopFailure {
		t.Fatalf("a reported limit was sent again: %+v", got)
	}

	// A StopFailure whose append failed leaves the limit to the session's end.
	env.Now = env.Now.Add(time.Minute)
	write(apiError(env.Now.Add(-time.Second), "overloaded"))
	spoolDir := filepath.Join(t.TempDir(), "spool")
	sp, err := spool.Open(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	env.Spool = sp
	if err := os.Remove(spoolDir); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"session_id": "funding-session", "cwd": env.Cwd, "transcript_path": path, "error": "rate_limit"})
	env.Stdin = strings.NewReader(string(b))
	_ = stopFailure(context.Background(), env)
	if err := os.Mkdir(spoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := limitRun(t, env, path, "", sessionEnd); len(got) != 1 || got[0].Attrs[semconv.TermaEvidenceSourceKey] != sourceClaudeTranscript {
		t.Fatalf("a failed append was taken as reported: %+v", got)
	}

	// A failure Claude Code gave no category stays without one.
	env.Now = env.Now.Add(time.Minute)
	write(apiError(env.Now.Add(-time.Second), "unknown"))
	if got := limitRun(t, env, path, "", stopFailure, sessionEnd); len(got) != 0 {
		t.Fatalf("the session's end made up a category: %+v", got)
	}
}
