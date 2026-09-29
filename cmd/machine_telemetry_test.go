package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/relay"
)

// setupSandbox is a signed-in developer with a home, Claude Code and Codex of the test's
// own, against the fake auth host.
func setupSandbox(t *testing.T) (*fakeAuth, string) {
	t.Helper()
	f := newFakeAuth(t)
	authSandbox(t, f)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	// Claude Code and Codex of the sandbox's own, first on PATH: status and doctor judge
	// only agents they find, and a CI runner has neither installed.
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "claude"), "#!/bin/sh\necho '2.1.284 (Claude Code)'\n")
	writeExecutable(t, filepath.Join(bin, "codex"), "#!/bin/sh\necho 'codex-cli 0.158.0'\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	return f, home
}

func setupRun(t *testing.T, extra ...string) string {
	t.Helper()
	args := append([]string{"setup", "--harness", "claude,codex", "--project", "Acme Web", "--no-browser", "--yes", "--no-statusline"}, extra...)
	out, err := within(20*time.Second).combined(t, args...)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	return out
}

// With a relay, setup points every agent's global configuration at it — OTLP/JSON, the
// relay's token and nothing that names a project — and records the machine project, whose
// key the relay delivers unbound repositories' sessions with.
func TestSetupPointsEveryAgentAtTheRelay(t *testing.T) {
	_, home := setupSandbox(t)
	r := fakeRelay(t)
	out := setupRun(t)
	if r.installed == "" {
		t.Fatalf("setup did not install the relay service:\n%s", out)
	}
	if !strings.Contains(out, "Relay") || !strings.Contains(out, r.config.Endpoint()) {
		t.Fatalf("setup did not say where the relay runs:\n%s", out)
	}
	acme := projectsIn(orgA().ID)[0]
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.Mode != config.TelemetryRelay || cfg.Telemetry.Project.ID != acme.ID || cfg.Telemetry.Project.Name != acme.Name {
		t.Fatalf("recorded telemetry: %+v", cfg.Telemetry)
	}
	for _, h := range []harness.Harness{harness.Claude{}, harness.Codex{}} {
		st, err := h.Status()
		if err != nil || !st.Connected || st.Endpoint != r.config.Endpoint() || len(st.Signals) != 3 {
			t.Fatalf("%s: %+v, %v", h.DisplayName(), st, err)
		}
		if keystore.GetFor(h.Name(), acme.ID) == "" {
			t.Fatalf("%s: no key stored for the machine project", h.DisplayName())
		}
	}
	codex, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`protocol = "json"`, "Bearer " + r.config.Token} {
		if !strings.Contains(string(codex), want) {
			t.Fatalf("Codex's configuration lacks %q:\n%s", want, codex)
		}
	}
	for _, never := range []string{"ter_srv_", harness.AttrProjectID} {
		if strings.Contains(string(codex), never) {
			t.Fatalf("Codex's configuration names %q, which is the relay's to decide:\n%s", never, codex)
		}
	}
	claude := readClaudeSettings(t, filepath.Join(home, ".claude", "settings.json"))
	if claude["OTEL_EXPORTER_OTLP_PROTOCOL"] != "http/json" || claude["OTEL_EXPORTER_OTLP_ENDPOINT"] != r.config.Endpoint() {
		t.Fatalf("Claude Code's configuration: %+v", claude)
	}
	helper, err := harness.RelayHelperFilePath(harness.Claude{})
	if err != nil {
		t.Fatal(err)
	}
	headers, err := exec.Command(helper).Output()
	if err != nil || !strings.Contains(string(headers), "Bearer "+r.config.Token) {
		t.Fatalf("Claude Code's headers helper printed %q, %v", headers, err)
	}

	// status and doctor agree: every agent through the relay, and the relay delivering.
	status, _ := runTerma(t, "status")
	for _, want := range []string{"Claude Code → connected (through terma's relay)", "Relay:       running on " + r.config.Endpoint()} {
		if !strings.Contains(status, want) {
			t.Fatalf("status should say %q:\n%s", want, status)
		}
	}
}

// --no-relay, or a machine where no relay can run, exports straight to Terma with the
// machine project's key. The choice is recorded, so a later setup keeps it.
func TestSetupWithoutARelayExportsStraightToTerma(t *testing.T) {
	setupSandbox(t)
	r := fakeRelay(t)
	setupRun(t, "--no-relay")
	if r.installed != "" {
		t.Fatal("--no-relay installed the relay service")
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.Mode != config.TelemetryDirect || !cfg.Telemetry.NoRelay {
		t.Fatalf("recorded telemetry: %+v", cfg.Telemetry)
	}
	acme := projectsIn(orgA().ID)[0]
	for _, h := range []harness.Harness{harness.Claude{}, harness.Codex{}} {
		st, err := h.Status()
		if err != nil || !st.Connected || st.Endpoint != cfg.OTLPURL {
			t.Fatalf("%s: %+v, %v", h.DisplayName(), st, err)
		}
	}
	if st, _ := (harness.Codex{}).Status(); st.ProjectID != acme.ID {
		t.Fatalf("Codex direct export names project %q, want %q", st.ProjectID, acme.ID)
	}
	setupRun(t)
	if cfg, _ := loadConfig(); cfg.Telemetry.Mode != config.TelemetryDirect || r.installed != "" {
		t.Fatalf("a later setup did not keep --no-relay: %+v", cfg.Telemetry)
	}
	setupRun(t, "--no-relay=false")
	if cfg, _ := loadConfig(); cfg.Telemetry.Mode != config.TelemetryRelay || r.installed == "" {
		t.Fatalf("--no-relay=false did not bring the relay back: %+v", cfg.Telemetry)
	}
	// Moving from direct to the relay replaces terma's own earlier settings without
	// asking for --force.
	if st, _ := (harness.Codex{}).Status(); st.Endpoint != r.config.Endpoint() {
		t.Fatalf("Codex still exports to %s", st.Endpoint)
	}
}

// A relay that cannot be installed must not leave the agents exporting to nothing: setup
// says so and points them straight at Terma.
func TestSetupFallsBackWhenTheRelayCannotStart(t *testing.T) {
	setupSandbox(t)
	fakeRelay(t)
	relayInstallService = func(context.Context, string) error { return errors.New("launchctl: permission denied") }
	out := setupRun(t)
	if !strings.Contains(out, "could not start (launchctl: permission denied)") {
		t.Fatalf("setup did not say the relay failed:\n%s", out)
	}
	cfg, _ := loadConfig()
	if cfg.Telemetry.Mode != config.TelemetryDirect {
		t.Fatalf("recorded telemetry: %+v", cfg.Telemetry)
	}
	if st, _ := (harness.Codex{}).Status(); !st.Connected || st.Endpoint != cfg.OTLPURL {
		t.Fatalf("Codex after a failed relay: %+v", st)
	}
}

// Another collector's settings in an agent's global file are the developer's: setup
// leaves that agent alone and says how to replace them, unless --force.
func TestSetupLeavesAnotherCollectorUnlessForced(t *testing.T) {
	_, home := setupSandbox(t)
	fakeRelay(t)
	codexConfig := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	other := "[otel]\nexporter = { otlp-http = { endpoint = \"https://collector.example.com/v1/logs\", protocol = \"binary\" } }\n"
	if err := os.WriteFile(codexConfig, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	out := setupRun(t)
	if !strings.Contains(out, "Codex") || !strings.Contains(out, "already exports elsewhere") || !strings.Contains(out, "terma setup --force") {
		t.Fatalf("setup did not explain the skipped agent:\n%s", out)
	}
	if data, _ := os.ReadFile(codexConfig); string(data) != other {
		t.Fatalf("setup changed another collector's settings:\n%s", data)
	}
	if st, _ := (harness.Claude{}).Status(); !st.Connected {
		t.Fatal("one agent's conflict stopped the other being configured")
	}
	setupRun(t, "--force")
	if data, _ := os.ReadFile(codexConfig); strings.Contains(string(data), "collector.example.com") {
		t.Fatalf("--force did not replace the other collector:\n%s", data)
	}
}

// The relay check follows the relay's own state, and applies only when an agent
// exports through it.
func TestRelayCheck(t *testing.T) {
	r := fakeRelay(t)
	through := []harnessVerdict{{displayName: "Codex", route: routeRelay}}
	if c := relayCheck(context.Background(), []harnessVerdict{{route: routeGlobal}}); c.Status != doctor.Skip {
		t.Fatalf("no agent through the relay: %+v", c)
	}
	if c := relayCheck(context.Background(), through); c.Status != doctor.Fail || c.Fix != "terma setup" {
		t.Fatalf("relay not set up: %+v", c)
	}
	r.installed = "terma"
	if c := relayCheck(context.Background(), through); c.Status != doctor.Pass || !strings.Contains(c.Detail, r.config.Endpoint()) {
		t.Fatalf("relay running: %+v", c)
	}
	r.health = relay.Health{Version: "test", HeldProjects: []string{"p1"}}
	if c := relayCheck(context.Background(), through); c.Status != doctor.Warn || !strings.Contains(c.Detail, "p1") {
		t.Fatalf("relay holding a project: %+v", c)
	}
	r.health = relay.Health{LastError: "401 Unauthorized"}
	if c := relayCheck(context.Background(), through); c.Status != doctor.Warn || !strings.Contains(c.Detail, "401") {
		t.Fatalf("relay failing: %+v", c)
	}
	r.running = false
	if c := relayCheck(context.Background(), through); c.Status != doctor.Fail || !strings.Contains(c.Detail, "not running") {
		t.Fatalf("relay stopped: %+v", c)
	}
}

func TestTermaEndpoint(t *testing.T) {
	rc := relay.Config{Port: 14399, Token: "t"}
	for value, want := range map[string]bool{
		"https://otel.example.com":              true, // this machine's ingest host
		"https://otel.example.com/v1/logs":      true,
		"http://127.0.0.1:14399/v1/traces":      true, // the relay
		"http://127.0.0.1:14318":                true, // the relay's default port
		"https://otel.terma.ai":                 true, // a built-in environment's host
		"https://collector.example.com/v1/logs": false,
		"":                                      false,
	} {
		if got := termaEndpoint(value, "https://otel.example.com", rc); got != want {
			t.Errorf("termaEndpoint(%q) = %v, want %v", value, got, want)
		}
	}
}
