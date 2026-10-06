package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// quotaPayload's empty cwd leaves the hook in statusEnv's repository.
const quotaPayload = `{"session_id":"sess-q","prompt_id":"p1","version":"2.1.272","cwd":"","model":{"id":"claude-haiku-4-5","display_name":"Haiku 4.5"},"fast_mode":false,"cost":{"total_cost_usd":0.0123},"context_window":{"used_percentage":12.5},"rate_limits":{"five_hour":{"used_percentage":3,"resets_at":1789483200},"seven_day":{"used_percentage":38,"resets_at":1789585200}}}`

func statusEnv(t *testing.T, stdin string) (hookrun.Env, *bytes.Buffer, *spool.Spool) {
	stateDir := t.TempDir()
	t.Helper()
	cwd := newRepo(t)
	sp, _ := spool.Open(t.TempDir())
	var out bytes.Buffer
	return hookrun.Env{StateDir: stateDir, Now: time.Now(), Cwd: cwd, Policy: hookruntest.Admitting(cwd), Team: hookruntest.Team, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: os.Stderr, Spool: sp, Version: "test"}, &out, sp
}

func TestStatusLinePassesBytesThroughUnchanged(t *testing.T) {
	in := "\x1b[32mgreen\x1b[0m\nline two\n{\"not\":\"json\""
	env, out, _ := statusEnv(t, in)
	code := statusLine(context.Background(), env, statusLineOptions{Renderer: "cat"})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !bytes.Equal(out.Bytes(), []byte(in)) {
		t.Fatalf("renderer saw %q, want %q", out.String(), in)
	}
}

func TestStatusLineReturnsRendererExitStatusAndStderr(t *testing.T) {
	env, out, _ := statusEnv(t, quotaPayload)
	var errBuf bytes.Buffer
	env.Stderr = &errBuf
	code := statusLine(context.Background(), env, statusLineOptions{Renderer: "printf drawn; echo oops >&2; exit 3"})
	if code != 3 || out.String() != "drawn" || !strings.Contains(errBuf.String(), "oops") {
		t.Fatalf("code %d out %q err %q", code, out.String(), errBuf.String())
	}
}

func TestStatusLineRendererKeepsOptionsOfItsOwn(t *testing.T) {
	env, out, _ := statusEnv(t, quotaPayload)
	t.Setenv("COLUMNS", "123")
	code := statusLine(context.Background(), env, statusLineOptions{Renderer: `printf "%s %s" "$COLUMNS" "$(pwd)"`})
	if code != 0 || out.String() != "123 "+env.Cwd {
		t.Fatalf("code %d out %q", code, out.String())
	}
}

func TestStatusLineSurvivesARendererThatIgnoresStdinAndAnOversizedPayload(t *testing.T) {
	big := strings.Repeat("x", statusLineMaxInput+4096)
	env, out, sp := statusEnv(t, big)
	code := statusLine(context.Background(), env, statusLineOptions{Renderer: "echo fine"})
	if code != 0 || out.String() != "fine\n" {
		t.Fatalf("code %d out %q", code, out.String())
	}
	if evs := hookruntest.Spooled(t, sp); len(evs) != 0 {
		t.Fatalf("oversized input must not be captured: %d events", len(evs))
	}
	env, out, _ = statusEnv(t, big)
	statusLine(context.Background(), env, statusLineOptions{Renderer: "wc -c | tr -d ' '"})
	if strings.TrimSpace(out.String()) != "1052672" {
		t.Fatalf("renderer got %s bytes", strings.TrimSpace(out.String()))
	}
}

func TestStatusLineCapturesQuotaOncePerChange(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	statusLine(context.Background(), env, statusLineOptions{CaptureOnly: true})
	env.Stdin = strings.NewReader(quotaPayload)
	statusLine(context.Background(), env, statusLineOptions{CaptureOnly: true})
	evs := hookruntest.Spooled(t, sp)
	if len(evs) != 1 {
		t.Fatalf("identical payloads must spool once, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Name != semconv.TermaSessionQuotaEvent || ev.SessionID != "sess-q" {
		t.Fatalf("event %+v", ev)
	}
	for k, want := range map[string]any{
		semconv.TermaRateLimitFiveHourUsedPercentKey: 3.0, semconv.TermaRateLimitSevenDayUsedPercentKey: 38.0,
		semconv.TermaRateLimitFiveHourResetsAtKey: 1789483200.0, semconv.GenAIRequestModelKey: "claude-haiku-4-5",
		semconv.GenAIMainAgentNameKey: "claude-code", semconv.TermaEvidenceSourceKey: sourceClaudeStatusline,
	} {
		if got := ev.Attrs[k]; got != want {
			t.Errorf("%s = %v (%T), want %v", k, got, got, want)
		}
	}
	for _, k := range []string{"cwd", "transcript_path", "session_name", "prompt_id", "fast_mode", "session_cost_usd", "claude.version", "time_basis"} {
		if _, ok := ev.Attrs[k]; ok {
			t.Errorf("%s must not be captured", k)
		}
	}

	changed := strings.Replace(quotaPayload, `"used_percentage":38`, `"used_percentage":41`, 1)
	env.Stdin = strings.NewReader(changed)
	statusLine(context.Background(), env, statusLineOptions{CaptureOnly: true})
	if evs := hookruntest.Spooled(t, sp); len(evs) != 1 || evs[0].Attrs[semconv.TermaRateLimitSevenDayUsedPercentKey] != 41.0 {
		t.Fatalf("a changed window must spool again: %+v", evs)
	}

	env.Now = env.Now.Add(hookrun.QuotaHeartbeat + time.Second)
	env.Stdin = strings.NewReader(changed)
	statusLine(context.Background(), env, statusLineOptions{CaptureOnly: true})
	if evs := hookruntest.Spooled(t, sp); len(evs) != 1 {
		t.Fatalf("heartbeat must re-send, got %d", len(evs))
	}
}

func TestStatusLineWithoutWindowsCapturesNothingButStillRenders(t *testing.T) {
	early := `{"session_id":"sess-e","cwd":"","model":{"id":"m"},"cost":{"total_cost_usd":0}}`
	env, out, sp := statusEnv(t, early)
	statusLine(context.Background(), env, statusLineOptions{Renderer: "cat"})
	if out.String() != early {
		t.Fatalf("render %q", out.String())
	}
	if evs := hookruntest.Spooled(t, sp); len(evs) != 0 {
		t.Fatalf("no windows must spool nothing: %+v", evs)
	}
	env, out, sp = statusEnv(t, "{oops")
	if code := statusLine(context.Background(), env, statusLineOptions{Renderer: "cat"}); code != 0 || out.String() != "{oops" {
		t.Fatalf("code %d out %q", code, out.String())
	}
	if evs := hookruntest.Spooled(t, sp); len(evs) != 0 {
		t.Fatal("malformed input must not spool")
	}
}

func TestStatusLineIndicatorPrefixesTheFirstLineOnly(t *testing.T) {
	t.Setenv("NO_COLOR", "1") // the mark without colour: "t "
	in := "\x1b[32mone\x1b[0m\ntwo"
	env, out, _ := statusEnv(t, in)
	statusLine(context.Background(), env, statusLineOptions{Renderer: "cat", Indicator: true})
	if got := out.String(); got != "t "+in {
		t.Fatalf("indicator output %q", got)
	}
	env, out, _ = statusEnv(t, in)
	statusLine(context.Background(), env, statusLineOptions{Renderer: "true", Indicator: true})
	if out.String() != "" {
		t.Fatalf("empty rendering %q", out.String())
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORTERM", "truecolor")
	env, out, _ = statusEnv(t, quotaPayload)
	statusLine(context.Background(), env, statusLineOptions{Renderer: "printf custom", Indicator: true})
	if got := out.String(); got != "\x1b[38;2;167;139;250mt\x1b[0m custom" {
		t.Fatalf("coloured mark %q", got)
	}
	env, _, _ = statusEnv(t, quotaPayload)
	if code := statusLine(context.Background(), env, statusLineOptions{Renderer: "exit 4", Indicator: true}); code != 4 {
		t.Fatalf("exit %d", code)
	}
}

func TestStatusLineWithoutRendererIsSilentButCaptures(t *testing.T) {
	for _, renderer := range []string{"", "  ", "terma hook statusline"} {
		t.Run(renderer, func(t *testing.T) {
			env, out, sp := statusEnv(t, quotaPayload)
			captured := 0
			code := statusLine(context.Background(), env, statusLineOptions{Renderer: renderer, Indicator: true, OnCapture: func() { captured++ }})
			if code != 0 || out.Len() != 0 {
				t.Fatalf("code=%d output=%q", code, out.String())
			}
			if evs := hookruntest.Spooled(t, sp); len(evs) != 1 || captured != 1 {
				t.Fatalf("events=%+v callbacks=%d", evs, captured)
			}
		})
	}
}

// A status line in an admitted repository, from a subdirectory too, stamps the developer's
// team; one in a repository the list does not name, or outside git, records nothing.
func TestStatusLineStampsProjectFromRepository(t *testing.T) {
	root := newRepo(t)
	cwd := filepath.Join(root, "nested")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	in := func(dir string) string {
		return strings.Replace(quotaPayload, `"cwd":""`, `"cwd":`+string(mustJSON(dir)), 1)
	}
	env, _, sp := statusEnv(t, in(cwd))
	env.Team, env.Policy = "proj_sl", hookruntest.Admitting(root)
	statusLine(context.Background(), env, statusLineOptions{CaptureOnly: true})
	evs := hookruntest.Spooled(t, sp)
	if len(evs) != 1 || evs[0].Attrs[hookrun.AttrProjectID] != "proj_sl" {
		t.Fatalf("project: %+v", evs)
	}
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	for _, tc := range []struct{ dir, entry string }{{cwd, "github.com/acme/elsewhere"}, {outside, "github.com/acme/" + filepath.Base(outside)}} {
		env.Stdin, env.Policy.Repositories = strings.NewReader(in(tc.dir)), []string{tc.entry}
		env.Now = env.Now.Add(time.Hour)
		statusLine(context.Background(), env, statusLineOptions{CaptureOnly: true})
		if evs := hookruntest.Spooled(t, sp); len(evs) != 0 {
			t.Fatalf("%s under %s recorded %+v", tc.dir, tc.entry, evs)
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// A rollover changes what is sent, so it is captured; a new prompt or a running cost is
// not sent, so it captures nothing new.
func TestStatusLineCapturesResetChangesOnly(t *testing.T) {
	for _, change := range []struct {
		name, from, to string
		want           int
	}{
		{"window rollover", `"resets_at":1789483200`, `"resets_at":1789501200`, 2},
		{"new prompt", `"prompt_id":"p1"`, `"prompt_id":"p2"`, 1},
		{"running cost", `"total_cost_usd":0.0123`, `"total_cost_usd":0.0246`, 1},
	} {
		t.Run(change.name, func(t *testing.T) {
			env, _, sp := statusEnv(t, quotaPayload)
			statusLine(context.Background(), env, statusLineOptions{CaptureOnly: true})
			changed := strings.Replace(quotaPayload, change.from, change.to, 1)
			for range 2 {
				env.Stdin = strings.NewReader(changed)
				env.Now = env.Now.Add(time.Second)
				statusLine(context.Background(), env, statusLineOptions{CaptureOnly: true})
			}
			if evs := hookruntest.Spooled(t, sp); len(evs) != change.want {
				t.Fatalf("want %d snapshots, got %+v", change.want, evs)
			}
		})
	}
}

func TestStatusLineDeliveryCallbackOnlyAfterCapture(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	calls := 0
	opts := statusLineOptions{CaptureOnly: true, OnCapture: func() {
		calls++
		if n, _, _ := sp.Pending(); n == 0 {
			t.Fatal("delivery started before append")
		}
	}}
	// Disabled capture must not update the dedup state either.
	disabled := env
	disabled.Spool = nil
	statusLine(context.Background(), disabled, opts)
	env.Stdin = strings.NewReader(quotaPayload)
	statusLine(context.Background(), env, opts)
	env.Stdin = strings.NewReader(quotaPayload)
	statusLine(context.Background(), env, opts)
	if calls != 1 {
		t.Fatalf("want one queued-snapshot callback, got %d", calls)
	}
}

func TestClaudeQuotaSequenceAndUnavailableTransition(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	var p statusLinePayload
	if err := json.Unmarshal([]byte(quotaPayload), &p); err != nil {
		t.Fatal(err)
	}
	if !captureQuota(env, &p) {
		t.Fatal("first observation missing")
	}
	if captureQuota(env, &p) {
		t.Fatal("duplicate redraw captured")
	}
	// Two changes within one prompt are both kept, even with identical observation times.
	w := p.RateLimits["five_hour"]
	pct := 100.0
	w.UsedPercentage = &pct
	p.RateLimits["five_hour"] = w
	if !captureQuota(env, &p) {
		t.Fatal("within-prompt change missing")
	}
	p.RateLimits = nil
	if !captureQuota(env, &p) {
		t.Fatal("unavailable transition missing")
	}
	evs := hookruntest.Spooled(t, sp)
	if len(evs) != 3 {
		t.Fatal(evs)
	}
	stream := evs[0].Attrs[semconv.TermaObservationStreamKey]
	for i, ev := range evs {
		if ev.Attrs[semconv.TermaObservationSequenceKey] != float64(i+1) || ev.Attrs[semconv.TermaObservationStreamKey] != stream || ev.Attrs[semconv.TermaObservationIDKey] == nil {
			t.Fatal(ev)
		}
		if _, ok := ev.Attrs[semconv.TermaObservationTimeKey]; ok {
			t.Fatal("invented provider timestamp")
		}
	}
	if evs[2].Attrs[semconv.TermaEvidenceStatusKey] != "unavailable" {
		t.Fatal(evs[2])
	}
}

func claudeConfigDir(t *testing.T, accountUUID string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		t.Setenv(k, "")
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount":{"accountUuid":"`+accountUUID+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeQuotaCarriesAccountID(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	claudeConfigDir(t, "account-q")
	var p statusLinePayload
	if err := json.Unmarshal([]byte(quotaPayload), &p); err != nil {
		t.Fatal(err)
	}
	if !captureQuota(env, &p) {
		t.Fatal("observation missing")
	}
	evs := hookruntest.Spooled(t, sp)
	if len(evs) != 1 || evs[0].Attrs[semconv.TermaAccountIDKey] != "account-q" {
		t.Fatalf("quota record: %+v", evs)
	}
	if _, ok := evs[0].Attrs[semconv.TermaAccountOrganizationIDKey]; ok {
		t.Fatalf("organization_id invented for a login that names none: %+v", evs[0].Attrs)
	}
}

// The quota record names the organization: a Team seat is funded by it, not the account.
func TestClaudeQuotaCarriesOrganizationID(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	claudeConfigDir(t, "unused")
	writeRealClaudeAccount(t)
	var p statusLinePayload
	if err := json.Unmarshal([]byte(quotaPayload), &p); err != nil {
		t.Fatal(err)
	}
	if !captureQuota(env, &p) {
		t.Fatal("observation missing")
	}
	evs := hookruntest.Spooled(t, sp)
	if len(evs) != 1 || evs[0].Attrs[semconv.TermaAccountIDKey] != realClaudeAccountID || evs[0].Attrs[semconv.TermaAccountOrganizationIDKey] != realClaudeOrganizationID {
		t.Fatalf("quota record: %+v", evs)
	}
}

// Under an override credential neither the OAuth account nor its organization is stamped.
func TestClaudeQuotaOmitsAccountIDUnderOverrideCredential(t *testing.T) {
	for _, override := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		t.Run(override, func(t *testing.T) {
			env, _, sp := statusEnv(t, quotaPayload)
			claudeConfigDir(t, "unused")
			writeRealClaudeAccount(t)
			t.Setenv(override, "1")
			var p statusLinePayload
			if err := json.Unmarshal([]byte(quotaPayload), &p); err != nil {
				t.Fatal(err)
			}
			if !captureQuota(env, &p) {
				t.Fatal("observation missing")
			}
			evs := hookruntest.Spooled(t, sp)
			if len(evs) != 1 {
				t.Fatalf("quota record: %+v", evs)
			}
			for _, k := range []string{semconv.TermaAccountIDKey, semconv.TermaAccountOrganizationIDKey} {
				if _, ok := evs[0].Attrs[k]; ok {
					t.Fatalf("%s stamped under override credential: %+v", k, evs[0].Attrs)
				}
			}
		})
	}
}

// Another session's /login leaves this session's account alone; its own /login switches it
// with an otherwise identical payload, as a fresh observation.
func TestClaudeQuotaAccountSwitchEmitsNewObservation(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	claudeConfigDir(t, "account-1")
	var p statusLinePayload
	if err := json.Unmarshal([]byte(quotaPayload), &p); err != nil {
		t.Fatal(err)
	}
	p.TranscriptPath = filepath.Join(t.TempDir(), "sess-q.jsonl")
	if !captureQuota(env, &p) {
		t.Fatal("first observation missing")
	}
	if captureQuota(env, &p) {
		t.Fatal("identical redraw captured")
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json"), []byte(`{"oauthAccount":{"accountUuid":"account-2"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if captureQuota(env, &p) {
		t.Fatalf("another session's login relabelled this one: %+v", hookruntest.Spooled(t, sp))
	}
	appendLogin(t, p.TranscriptPath, p.SessionID, env.Now.Add(time.Second))
	if !captureQuota(env, &p) {
		t.Fatal("account switch not captured")
	}
	evs := hookruntest.Spooled(t, sp)
	if len(evs) != 2 || evs[0].Attrs[semconv.TermaAccountIDKey] != "account-1" || evs[1].Attrs[semconv.TermaAccountIDKey] != "account-2" {
		t.Fatalf("account switch observations: %+v", evs)
	}
}

// An organization switch keeps the account id and still emits a fresh observation.
func TestClaudeQuotaOrganizationSwitchEmitsNewObservation(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	claudeConfigDir(t, "unused")
	login := func(org string) {
		t.Helper()
		body := `{"oauthAccount":{"accountUuid":"account-1","organizationUuid":"` + org + `"}}`
		if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	login("org-team")
	var p statusLinePayload
	if err := json.Unmarshal([]byte(quotaPayload), &p); err != nil {
		t.Fatal(err)
	}
	p.TranscriptPath = filepath.Join(t.TempDir(), "sess-q.jsonl")
	if !captureQuota(env, &p) {
		t.Fatal("first observation missing")
	}
	if captureQuota(env, &p) {
		t.Fatal("identical redraw captured")
	}
	login("org-personal")
	appendLogin(t, p.TranscriptPath, p.SessionID, env.Now.Add(time.Second))
	if !captureQuota(env, &p) {
		t.Fatal("organization switch not captured")
	}
	evs := hookruntest.Spooled(t, sp)
	if len(evs) != 2 || evs[0].Attrs[semconv.TermaAccountOrganizationIDKey] != "org-team" || evs[1].Attrs[semconv.TermaAccountOrganizationIDKey] != "org-personal" ||
		evs[1].Attrs[semconv.TermaAccountIDKey] != "account-1" {
		t.Fatalf("organization switch observations: %+v", evs)
	}
}

func TestStatusLineTimeoutConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", 30 * time.Second},
		{"invalid", 30 * time.Second},
		{"0", 30 * time.Second},
		{"-1s", 30 * time.Second},
		{"2m", 2 * time.Minute},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("TERMA_STATUSLINE_TIMEOUT", tc.value)
			if got := statusLineRendererTimeout(0); got != tc.want {
				t.Fatalf("timeout=%s want=%s", got, tc.want)
			}
			if got := statusLineRendererTimeout(time.Second); got != time.Second {
				t.Fatalf("explicit override ignored: %s", got)
			}
		})
	}
}
