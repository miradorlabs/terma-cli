package hookrun

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/spool"
)

const quotaPayload = `{"session_id":"sess-q","prompt_id":"p1","version":"2.1.272","cwd":"/tmp","model":{"id":"claude-haiku-4-5","display_name":"Haiku 4.5"},"fast_mode":false,"cost":{"total_cost_usd":0.0123},"context_window":{"used_percentage":12.5},"rate_limits":{"five_hour":{"used_percentage":3,"resets_at":1789483200},"seven_day":{"used_percentage":38,"resets_at":1789585200}}}`

func statusEnv(t *testing.T, stdin string) (Env, *bytes.Buffer, *spool.Spool) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	sp, _ := spool.Open(t.TempDir())
	var out bytes.Buffer
	// Resolve symlinks so Cwd matches what a renderer's own `pwd` reports: on macOS
	// t.TempDir() is under /var, a symlink to /private/var, and `pwd` returns the latter.
	cwd := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	return Env{Now: time.Now(), Cwd: cwd, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: os.Stderr, Spool: sp, Version: "test"}, &out, sp
}

func spooledQuota(t *testing.T, sp *spool.Spool) []spool.Event {
	t.Helper()
	var got []spool.Event
	sp.Flush(context.Background(), spool.SenderFunc(func(_ context.Context, evs []spool.Event) ([]spool.Event, error) {
		got = append(got, evs...)
		return nil, nil
	}), spool.FlushOptions{Force: true})
	return got
}

func TestStatusLinePassesBytesThroughUnchanged(t *testing.T) {
	// ANSI, a multi-line payload and no trailing newline: what goes in comes out.
	in := "\x1b[32mgreen\x1b[0m\nline two\n{\"not\":\"json\""
	env, out, _ := statusEnv(t, in)
	code := StatusLine(context.Background(), env, StatusLineOptions{Renderer: "cat"})
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
	code := StatusLine(context.Background(), env, StatusLineOptions{Renderer: "printf drawn; echo oops >&2; exit 3"})
	if code != 3 || out.String() != "drawn" || !strings.Contains(errBuf.String(), "oops") {
		t.Fatalf("code %d out %q err %q", code, out.String(), errBuf.String())
	}
}

func TestStatusLineRendererKeepsOptionsOfItsOwn(t *testing.T) {
	// The renderer sees the same environment and directory the hook got.
	env, out, _ := statusEnv(t, quotaPayload)
	t.Setenv("COLUMNS", "123")
	code := StatusLine(context.Background(), env, StatusLineOptions{Renderer: `printf "%s %s" "$COLUMNS" "$(pwd)"`})
	if code != 0 || out.String() != "123 "+env.Cwd {
		t.Fatalf("code %d out %q", code, out.String())
	}
}

func TestStatusLineSurvivesARendererThatIgnoresStdinAndAnOversizedPayload(t *testing.T) {
	big := strings.Repeat("x", statusLineMaxInput+4096)
	env, out, sp := statusEnv(t, big)
	code := StatusLine(context.Background(), env, StatusLineOptions{Renderer: "echo fine"})
	if code != 0 || out.String() != "fine\n" {
		t.Fatalf("code %d out %q", code, out.String())
	}
	if evs := spooledQuota(t, sp); len(evs) != 0 {
		t.Fatalf("oversized input must not be captured: %d events", len(evs))
	}
	// And every byte still reaches a renderer that does read it.
	env, out, _ = statusEnv(t, big)
	StatusLine(context.Background(), env, StatusLineOptions{Renderer: "wc -c | tr -d ' '"})
	if strings.TrimSpace(out.String()) != "1052672" {
		t.Fatalf("renderer got %s bytes", strings.TrimSpace(out.String()))
	}
}

func TestStatusLineCapturesQuotaOncePerChange(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	StatusLine(context.Background(), env, StatusLineOptions{CaptureOnly: true})
	env.Stdin = strings.NewReader(quotaPayload)
	StatusLine(context.Background(), env, StatusLineOptions{CaptureOnly: true})
	evs := spooledQuota(t, sp)
	if len(evs) != 1 {
		t.Fatalf("identical payloads must spool once, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Name != EventSessionQuota || ev.SessionID != "sess-q" {
		t.Fatalf("event %+v", ev)
	}
	for k, want := range map[string]any{"five_hour_used_pct": 3.0, "seven_day_used_pct": 38.0, "five_hour_resets_at": 1789483200.0, "fast_mode": false, "model": "claude-haiku-4-5", "prompt_id": "p1", "session_cost_usd": 0.0123, "claude.version": "2.1.272", "tool": "claude-code"} {
		if got := ev.Attrs[k]; got != want {
			t.Errorf("%s = %v (%T), want %v", k, got, got, want)
		}
	}
	for _, k := range []string{"cwd", "transcript_path", "session_name"} {
		if _, ok := ev.Attrs[k]; ok {
			t.Errorf("%s must not be captured", k)
		}
	}

	changed := strings.Replace(quotaPayload, `"used_percentage":38`, `"used_percentage":41`, 1)
	env.Stdin = strings.NewReader(changed)
	StatusLine(context.Background(), env, StatusLineOptions{CaptureOnly: true})
	if evs := spooledQuota(t, sp); len(evs) != 1 || evs[0].Attrs["seven_day_used_pct"] != 41.0 {
		t.Fatalf("a changed window must spool again: %+v", evs)
	}

	// The heartbeat re-sends an unchanged snapshot after ten minutes.
	env.Now = env.Now.Add(quotaHeartbeat + time.Second)
	env.Stdin = strings.NewReader(changed)
	StatusLine(context.Background(), env, StatusLineOptions{CaptureOnly: true})
	if evs := spooledQuota(t, sp); len(evs) != 1 {
		t.Fatalf("heartbeat must re-send, got %d", len(evs))
	}
}

func TestStatusLineWithoutWindowsCapturesNothingButStillRenders(t *testing.T) {
	early := `{"session_id":"sess-e","cwd":"/tmp","model":{"id":"m"},"cost":{"total_cost_usd":0}}`
	env, out, sp := statusEnv(t, early)
	StatusLine(context.Background(), env, StatusLineOptions{Renderer: "cat"})
	if out.String() != early {
		t.Fatalf("render %q", out.String())
	}
	if evs := spooledQuota(t, sp); len(evs) != 0 {
		t.Fatalf("no windows and no fast flag must spool nothing: %+v", evs)
	}
	// Malformed JSON: rendered, not captured, exit 0.
	env, out, sp = statusEnv(t, "{oops")
	if code := StatusLine(context.Background(), env, StatusLineOptions{Renderer: "cat"}); code != 0 || out.String() != "{oops" {
		t.Fatalf("code %d out %q", code, out.String())
	}
	if evs := spooledQuota(t, sp); len(evs) != 0 {
		t.Fatal("malformed input must not spool")
	}
}

func TestStatusLineIndicatorPrefixesTheFirstLineOnly(t *testing.T) {
	t.Setenv("NO_COLOR", "1") // the mark without colour: "t "
	in := "\x1b[32mone\x1b[0m\ntwo"
	env, out, _ := statusEnv(t, in)
	StatusLine(context.Background(), env, StatusLineOptions{Renderer: "cat", Indicator: true})
	if got := out.String(); got != "t "+in {
		t.Fatalf("indicator output %q", got)
	}
	// A renderer that intentionally hides itself must stay hidden.
	env, out, _ = statusEnv(t, in)
	StatusLine(context.Background(), env, StatusLineOptions{Renderer: "true", Indicator: true})
	if out.String() != "" {
		t.Fatalf("empty rendering %q", out.String())
	}
	// With colour on, the mark carries the brand escape and a reset.
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORTERM", "truecolor")
	env, out, _ = statusEnv(t, quotaPayload)
	StatusLine(context.Background(), env, StatusLineOptions{Renderer: "printf custom", Indicator: true})
	if got := out.String(); got != "\x1b[38;2;139;108;255mt\x1b[0m custom" {
		t.Fatalf("coloured mark %q", got)
	}
	// The renderer's exit status still comes through with the indicator on.
	env, _, _ = statusEnv(t, quotaPayload)
	if code := StatusLine(context.Background(), env, StatusLineOptions{Renderer: "exit 4", Indicator: true}); code != 4 {
		t.Fatalf("exit %d", code)
	}
}

func TestStatusLineWithoutRendererIsSilentButCaptures(t *testing.T) {
	for _, renderer := range []string{"", "  ", "terma hook statusline"} {
		t.Run(renderer, func(t *testing.T) {
			env, out, sp := statusEnv(t, quotaPayload)
			captured := 0
			code := StatusLine(context.Background(), env, StatusLineOptions{Renderer: renderer, Indicator: true, OnCapture: func() { captured++ }})
			if code != 0 || out.Len() != 0 {
				t.Fatalf("code=%d output=%q", code, out.String())
			}
			if evs := spooledQuota(t, sp); len(evs) != 1 || captured != 1 {
				t.Fatalf("events=%+v callbacks=%d", evs, captured)
			}
		})
	}
}

func TestStatusLineStampsProjectFromRepository(t *testing.T) {
	for _, nonGit := range []bool{false, true} {
		t.Run(map[bool]string{false: "git", true: "non_git"}[nonGit], func(t *testing.T) {
			root := t.TempDir()
			if !nonGit {
				root = initRepo(t)
			}
			writeFile(t, root, ".terma/settings.json", `{"project":{"id":"proj_sl"}}`)
			nested := filepath.Join(root, "nested")
			if err := os.MkdirAll(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			payload := strings.Replace(quotaPayload, `"cwd":"/tmp"`, `"cwd":`+string(mustJSON(nested)), 1)
			env, _, sp := statusEnv(t, payload)
			StatusLine(context.Background(), env, StatusLineOptions{CaptureOnly: true})
			evs := spooledQuota(t, sp)
			if len(evs) != 1 || evs[0].Attrs[AttrProjectID] != "proj_sl" || evs[0].Repo != filepath.Base(root) {
				t.Fatalf("project binding: %+v", evs)
			}
		})
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestStatusLineCapturesPromptResetAndCostChanges(t *testing.T) {
	for _, change := range []struct{ name, from, to string }{
		{"new prompt", `"prompt_id":"p1"`, `"prompt_id":"p2"`},
		{"window rollover", `"resets_at":1789483200`, `"resets_at":1789501200`},
		{"running cost", `"total_cost_usd":0.0123`, `"total_cost_usd":0.0246`},
	} {
		t.Run(change.name, func(t *testing.T) {
			env, _, sp := statusEnv(t, quotaPayload)
			StatusLine(context.Background(), env, StatusLineOptions{CaptureOnly: true})
			changed := strings.Replace(quotaPayload, change.from, change.to, 1)
			for range 2 {
				env.Stdin = strings.NewReader(changed)
				env.Now = env.Now.Add(time.Second)
				StatusLine(context.Background(), env, StatusLineOptions{CaptureOnly: true})
			}
			if evs := spooledQuota(t, sp); len(evs) != 2 {
				t.Fatalf("change must emit once despite unchanged percentages: %+v", evs)
			}
		})
	}
}

func TestStatusLineDeliveryCallbackOnlyAfterCapture(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	calls := 0
	opts := StatusLineOptions{CaptureOnly: true, OnCapture: func() {
		calls++
		if n, _, _ := sp.Pending(); n == 0 {
			t.Fatal("delivery started before append")
		}
	}}
	// Disabled capture must not update the dedup state either.
	disabled := env
	disabled.Spool = nil
	StatusLine(context.Background(), disabled, opts)
	env.Stdin = strings.NewReader(quotaPayload)
	StatusLine(context.Background(), env, opts)
	env.Stdin = strings.NewReader(quotaPayload)
	StatusLine(context.Background(), env, opts)
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
	if !env.captureQuota(&p) {
		t.Fatal("first observation missing")
	}
	if env.captureQuota(&p) {
		t.Fatal("duplicate redraw captured")
	}
	// Preserve two changes during one prompt, even with identical observation times.
	w := p.RateLimits["five_hour"]
	pct := 100.0
	w.UsedPercentage = &pct
	p.RateLimits["five_hour"] = w
	if !env.captureQuota(&p) {
		t.Fatal("within-prompt change missing")
	}
	p.RateLimits = nil
	p.FastMode = nil
	if !env.captureQuota(&p) {
		t.Fatal("unavailable transition missing")
	}
	evs := spooledQuota(t, sp)
	if len(evs) != 3 {
		t.Fatal(evs)
	}
	stream := evs[0].Attrs["source_stream"]
	for i, ev := range evs {
		if ev.Attrs["observation_sequence"] != float64(i+1) || ev.Attrs["source_stream"] != stream || ev.Attrs["prompt_id"] != p.PromptID || ev.Attrs["observation_id"] == nil {
			t.Fatal(ev)
		}
		if _, ok := ev.Attrs["source_time"]; ok {
			t.Fatal("invented provider timestamp")
		}
	}
	if evs[2].Attrs["evidence_status"] != "unavailable" {
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

func TestClaudeQuotaCarriesAccountIDAndSchemaV2(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	claudeConfigDir(t, "account-q")
	var p statusLinePayload
	if err := json.Unmarshal([]byte(quotaPayload), &p); err != nil {
		t.Fatal(err)
	}
	if !env.captureQuota(&p) {
		t.Fatal("observation missing")
	}
	evs := spooledQuota(t, sp)
	if len(evs) != 1 || evs[0].Attrs["account_id"] != "account-q" || evs[0].Attrs["schema_version"] != float64(1) {
		t.Fatalf("quota record: %+v", evs)
	}
	if _, ok := evs[0].Attrs["organization_id"]; ok {
		t.Fatalf("organization_id invented for a login that names none: %+v", evs[0].Attrs)
	}
}

// The quota record names the organization the account is signed in to, read from the
// real ~/.claude.json oauthAccount shape: a Team seat is funded by the organization,
// not the account, so the backend keys the funding facility on both.
func TestClaudeQuotaCarriesOrganizationID(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	claudeConfigDir(t, "unused")
	writeRealClaudeAccount(t)
	var p statusLinePayload
	if err := json.Unmarshal([]byte(quotaPayload), &p); err != nil {
		t.Fatal(err)
	}
	if !env.captureQuota(&p) {
		t.Fatal("observation missing")
	}
	evs := spooledQuota(t, sp)
	if len(evs) != 1 || evs[0].Attrs["account_id"] != realClaudeAccountID || evs[0].Attrs["organization_id"] != realClaudeOrganizationID {
		t.Fatalf("quota record: %+v", evs)
	}
}

// An env API key, auth token or cloud provider outranks the cached OAuth login,
// so neither the OAuth accountUuid nor its organization is stamped as the funding owner.
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
			if !env.captureQuota(&p) {
				t.Fatal("observation missing")
			}
			evs := spooledQuota(t, sp)
			if len(evs) != 1 || evs[0].Attrs["schema_version"] != float64(1) {
				t.Fatalf("quota record: %+v", evs)
			}
			for _, k := range []string{"account_id", "organization_id"} {
				if _, ok := evs[0].Attrs[k]; ok {
					t.Fatalf("%s stamped under override credential: %+v", k, evs[0].Attrs)
				}
			}
		})
	}
}

// A mid-session account switch with an otherwise-identical quota payload must
// emit a fresh observation, so the latest record names the new account.
func TestClaudeQuotaAccountSwitchEmitsNewObservation(t *testing.T) {
	env, _, sp := statusEnv(t, quotaPayload)
	claudeConfigDir(t, "account-1")
	var p statusLinePayload
	if err := json.Unmarshal([]byte(quotaPayload), &p); err != nil {
		t.Fatal(err)
	}
	if !env.captureQuota(&p) {
		t.Fatal("first observation missing")
	}
	if env.captureQuota(&p) {
		t.Fatal("identical redraw captured")
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json"), []byte(`{"oauthAccount":{"accountUuid":"account-2"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !env.captureQuota(&p) {
		t.Fatal("account switch not captured")
	}
	evs := spooledQuota(t, sp)
	if len(evs) != 2 || evs[0].Attrs["account_id"] != "account-1" || evs[1].Attrs["account_id"] != "account-2" {
		t.Fatalf("account switch observations: %+v", evs)
	}
}

// Switching organization keeps the account id (Team to personal on one login), so an
// otherwise-identical quota payload must still emit a fresh observation naming the new
// organization.
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
	if !env.captureQuota(&p) {
		t.Fatal("first observation missing")
	}
	if env.captureQuota(&p) {
		t.Fatal("identical redraw captured")
	}
	login("org-personal")
	if !env.captureQuota(&p) {
		t.Fatal("organization switch not captured")
	}
	evs := spooledQuota(t, sp)
	if len(evs) != 2 || evs[0].Attrs["organization_id"] != "org-team" || evs[1].Attrs["organization_id"] != "org-personal" ||
		evs[1].Attrs["account_id"] != "account-1" {
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
