package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// The command tree is the whole user-facing surface; a subcommand lost in a refactor
// would not fail any other test here.
func TestTelemetryCommandTree(t *testing.T) {
	root := NewRootCommand()

	for _, path := range []string{"telemetry connect", "telemetry status", "telemetry disconnect"} {
		fields := strings.Fields(path)
		cmd, _, err := root.Find(fields)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		// Find falls back to the closest parent when a leaf is missing, so the resolved
		// name has to be checked rather than trusted.
		if leaf := fields[len(fields)-1]; cmd.Name() != leaf {
			t.Fatalf("`terma %s` resolves to %q — the command does not exist", path, cmd.CommandPath())
		}
		if cmd.RunE == nil && cmd.Run == nil {
			t.Errorf("`terma %s` exists but does nothing", path)
		}
	}
}

// Every capture flag is a privacy decision. A flag renamed or dropped silently would
// change what leaves the machine.
func TestTelemetryConnectFlags(t *testing.T) {
	root := NewRootCommand()
	cmd, _, err := root.Find([]string{"telemetry", "connect"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}

	for _, name := range []string{"signals", "exclude-prompts", "exclude-tool-content", "key-name", "api-key", "yes", "scope"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("`telemetry connect` has no --%s flag", name)
		}
	}

	// Capture is the default; the exclusion flags default to false so a bare connect
	// exports everything, and redaction is the explicit choice.
	for _, name := range []string{"exclude-prompts", "exclude-tool-content"} {
		if got := cmd.Flags().Lookup(name).DefValue; got != "false" {
			t.Errorf("--%s defaults to %q, want false — a bare connect captures content", name, got)
		}
	}
}

// A harness typo must be a clear local error, not a request or a silent no-op.
func TestTelemetryRejectsUnknownHarness(t *testing.T) {
	_, err := runTerma(t, "telemetry", "connect", "gemini", "--yes")
	if err == nil {
		t.Fatal("connect accepted an unknown harness")
	}
	if !strings.Contains(err.Error(), "gemini") {
		t.Errorf("error = %q, want it to name the harness", err)
	}
	// The message should point at what is available rather than just refusing.
	for _, name := range harness.Names() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error = %q, want it to list the supported harness %q", err, name)
		}
	}
}

func TestTelemetryConnectRequiresAHarness(t *testing.T) {
	if _, err := runTerma(t, "telemetry", "connect"); err == nil {
		t.Fatal("connect ran without naming a harness")
	}
}

// `--signals` is validated before anything is minted or written: a typo'd signal would
// otherwise leave a harness looking connected and emitting nothing.
func TestTelemetryRejectsUnknownSignalBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	_, err := runTerma(t,
		"telemetry", "connect", "claude",
		"--signals", "spans",
		"--api-key", "mir_srv_test",
		"--project", "770e8400-e29b-41d4-a716-446655440000",
		"--yes",
	)
	if err == nil {
		t.Fatal("connect accepted an unknown signal")
	}
	if !strings.Contains(err.Error(), "spans") {
		t.Errorf("error = %q, want it to name the bad signal", err)
	}

	// Nothing should have been written on the way to that error.
	h := harness.Claude{}
	st, statusErr := h.Status()
	if statusErr != nil {
		t.Fatalf("Status: %v", statusErr)
	}
	if st.Exists {
		t.Fatal("a settings file was created despite the command failing validation")
	}
}

// --api-key installs a credential verbatim; a value that is not a server key would be
// written into the config and fail only later, at export time.
func TestTelemetryRejectsNonServerKey(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	_, err := runTerma(t,
		"telemetry", "connect", "claude",
		"--api-key", "mir_cli_not_a_server_key",
		"--project", "770e8400-e29b-41d4-a716-446655440000",
		"--yes",
	)
	if err == nil {
		t.Fatal("connect accepted a CLI token as --api-key")
	}
	if !strings.Contains(err.Error(), "mir_srv_") {
		t.Errorf("error = %q, want it to name the expected prefix", err)
	}
}

// status must work without a credential — it only reads local files — so it stays
// usable when a login has expired.
func TestTelemetryStatusNeedsNoCredential(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	out, _, err := termaRun{}.exec(t, "telemetry", "status", "-o", "json")
	if err != nil {
		t.Fatalf("status failed without a credential: %v", err)
	}
	if !strings.Contains(out, `"harness": "claude"`) {
		t.Errorf("status output did not report the claude harness:\n%s", out)
	}
}

func TestTelemetryStatusReportsCodexNotConnected(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	out, _, err := termaRun{}.exec(t, "telemetry", "status", "codex", "-o", "json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, `"state": "not connected"`) {
		t.Errorf("codex in an empty sandbox was not reported as not connected:\n%s", out)
	}
}

// The Codex lifecycle end to end: connect writes the key and funding notifier,
// status reads the export back, and disconnect restores the file byte for byte.
func TestTelemetryCodexConnectStatusDisconnect(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	const seed = "# my config\nmodel = \"gpt-5\"\n\n[mcp_servers.foo]\ncommand = \"foo\"\n"
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	run := func(args ...string) (string, error) {
		out, _, err := termaRun{}.exec(t, args...)
		return out, err
	}

	out, err := run("telemetry", "connect", "codex",
		"--api-key", "mir_srv_codex123",
		"--project", "770e8400-e29b-41d4-a716-446655440000",
		"--yes")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	if !strings.Contains(out, "written into this file") {
		t.Errorf("the plan did not say the key goes inline:\n%s", out)
	}
	if !strings.Contains(out, "`terma session list`") {
		t.Errorf("connect did not say where the sessions show up:\n%s", out)
	}
	if strings.Contains(out, "helpers") {
		t.Errorf("a headers helper was mentioned for a harness that has none:\n%s", out)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "# my config\nmodel = \"gpt-5\"") ||
		!strings.Contains(string(data), "[mcp_servers.foo]\ncommand = \"foo\"") ||
		!strings.Contains(string(data), "mir_srv_codex123") ||
		!strings.Contains(string(data), `notify = ["terma", "hook", "codex-notify"]`) {
		t.Fatalf("config.toml after connect:\n%s", data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm()&0o077 != 0 {
		t.Errorf("config.toml mode = %#o, want it tightened", info.Mode().Perm())
	}

	out, err = run("telemetry", "status", "codex", "-o", "json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, `"state": "connected"`) || !strings.Contains(out, `"project_id": "770e8400-e29b-41d4-a716-446655440000"`) {
		t.Errorf("status after connect:\n%s", out)
	}
	if strings.Contains(out, "mir_srv_codex123") {
		t.Error("status printed the whole key")
	}

	if _, err := run("telemetry", "disconnect", "codex", "--yes"); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != seed {
		t.Fatalf("config.toml after disconnect:\n%s\nwant the original:\n%s", data, seed)
	}
}

// With analytics disabled Codex sends no metrics whatever the exporter says. A full
// connect must refuse rather than report metrics connected; without metrics it works.
func TestTelemetryCodexRefusesMetricsWhenAnalyticsDisabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	const seed = "[analytics]\nenabled = false\n"
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	connect := func(args ...string) (string, error) {
		out, _, err := termaRun{}.exec(t, append([]string{
			"telemetry", "connect", "codex",
			"--api-key", "mir_srv_secret",
			"--project", "770e8400-e29b-41d4-a716-446655440000",
			"--yes",
		}, args...)...)
		return out, err
	}

	out, err := connect()
	if err == nil || !strings.Contains(err.Error(), "analytics.enabled") {
		t.Fatalf("connect error = %v, want analytics.enabled named", err)
	}
	if !strings.Contains(out, "--signals traces,logs") {
		t.Errorf("the conflict did not point at the way out:\n%s", out)
	}
	if data, _ := os.ReadFile(path); string(data) != seed {
		t.Fatalf("the config was modified despite the refusal:\n%s", data)
	}

	if out, err := connect("--force"); err == nil {
		t.Fatalf("--force flipped the user's analytics opt-out:\n%s", out)
	}
	if _, err := connect("--signals", "traces,logs"); err != nil {
		t.Fatalf("connect without metrics: %v", err)
	}
}

// A dormant profile that overturns the privacy posture — or points a signal elsewhere —
// is reported before the confirmation, with the profile named, but does not block a
// connect that the plain configuration asked for; status likewise lists it as a warning
// rather than calling the harness overridden.
func TestTelemetryCodexProfileOverridesAreReportedNotBlocking(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	profile := "[analytics]\nenabled = false\n\n[otel]\nexporter = \"none\"\nlog_user_prompt = true\ntool_result = { max_bytes = 2048 }\n"
	if err := os.WriteFile(filepath.Join(dir, "work.config.toml"), []byte(profile), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, _, err := termaRun{}.exec(t,
		"telemetry", "connect", "codex",
		"--api-key", "mir_srv_profiles",
		"--project", "770e8400-e29b-41d4-a716-446655440000",
		"--exclude-prompts", "--exclude-tool-content",
		"--yes",
	)
	if err != nil {
		t.Fatalf("a dormant profile blocked the connect: %v\n%s", err, out)
	}
	got := out
	if !strings.Contains(got, "reported, not blocking") {
		t.Errorf("the plan did not introduce the profile overrides as advisory:\n%s", got)
	}
	for _, key := range []string{
		"work.config.toml:otel.exporter=none",
		"work.config.toml:otel.log_user_prompt=true",
		"work.config.toml:otel.tool_result.max_bytes=2048",
		"work.config.toml:analytics.enabled=false",
	} {
		if !strings.Contains(got, key) {
			t.Errorf("the plan did not name %s:\n%s", key, got)
		}
	}
	if !strings.Contains(got, "--profile work") {
		t.Errorf("the plan did not say which profile selects the overrides:\n%s", got)
	}

	out, _, err = termaRun{}.exec(t, "telemetry", "status", "codex", "-o", "json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, `"state": "connected"`) || strings.Contains(out, "overridden") {
		t.Errorf("status treated a dormant profile as an override:\n%s", out)
	}
	if !strings.Contains(out, `"warnings"`) || !strings.Contains(out, "work.config.toml:otel.log_user_prompt") {
		t.Errorf("status did not list the profile overrides as warnings:\n%s", out)
	}
}

// A conflict key can carry a file name, and a file name can carry anything. What
// reaches the terminal must be stripped of control characters, key included.
func TestPrintConflictsSanitizesEveryField(t *testing.T) {
	var out bytes.Buffer
	printConflicts(&out, []harness.Conflict{{
		Key:    "evil\x1b[31m.config.toml:otel.exporter",
		Value:  "https://x\x1b[0m",
		Reason: "because\x07",
		Scope:  harness.ScopeProfile + "\x1b[2J",
	}}, false)
	if strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Fatalf("control characters reached the output:\n%q", out.String())
	}
	if !strings.Contains(out.String(), "evil[31m.config.toml:otel.exporter") {
		t.Errorf("the key was not printed with only its control characters removed:\n%q", out.String())
	}
}

// --inline-key is Claude's opt-out of the helper; for Codex inline is the only mode,
// and the flag must be accepted rather than rejected as inapplicable.
func TestTelemetryCodexInlineKeyFlagIsAccepted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	if _, err := runTerma(t,
		"telemetry", "connect", "codex",
		"--api-key", "mir_srv_inline",
		"--project", "770e8400-e29b-41d4-a716-446655440000",
		"--inline-key", "--yes",
	); err != nil {
		t.Fatalf("connect: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "mir_srv_inline") {
		t.Fatal("the key was not written")
	}
}

// The P1: a pre-existing per-signal endpoint would keep exporting to whoever owns it
// while inheriting the Authorization header Terma writes for the generic endpoint.
// Connect must refuse before minting a key or writing anything.
func TestTelemetryConnectRefusesOnPerSignalConflict(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	const original = `{"env":{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://other-collector.example.com"}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(original), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, _, err := termaRun{}.exec(t,
		"telemetry", "connect", "claude",
		"--api-key", "mir_srv_secret",
		"--project", "770e8400-e29b-41d4-a716-446655440000",
		"--yes",
	)
	if err == nil {
		t.Fatal("connect proceeded with a conflicting per-signal endpoint")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error = %q, want it to name the escape hatch", err)
	}
	// The plan must name the offending variable so the user can go and look at it.
	if !strings.Contains(out, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") {
		t.Errorf("output did not name the conflicting variable:\n%s", out)
	}

	// Nothing may have been written — above all, not the key.
	after, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(after) != original {
		t.Fatalf("the config was modified despite the refusal:\n%s", after)
	}
	if strings.Contains(string(after), "mir_srv_secret") {
		t.Fatal("the server key was written into a config that would have leaked it")
	}
}

func TestTelemetryConnectProceedsWithForce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"env":{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://other-collector.example.com"}}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := runTerma(t,
		"telemetry", "connect", "claude",
		"--api-key", "mir_srv_secret",
		"--project", "770e8400-e29b-41d4-a716-446655440000",
		"--yes", "--force",
	); err != nil {
		t.Fatalf("connect with --force failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "other-collector.example.com") {
		t.Fatal("--force left the conflicting endpoint in place alongside the key")
	}
}

// --identity none is the only way to keep a real email address out of a global config.
// --identity is for Codex and OpenCode. Claude Code identifies the person itself
// (user.id and user.email on every metric and event), and OTEL_RESOURCE_ATTRIBUTES is
// the user's own variable, so whatever the flag says, nothing of it reaches Claude
// Code's settings file.
func TestTelemetryIdentityFlagNeverReachesClaude(t *testing.T) {
	for _, tc := range []struct{ name, identity string }{
		{name: "default", identity: ""},
		{name: "explicit", identity: "team@example.com"},
		{name: "none", identity: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

			args := []string{
				"telemetry", "connect", "claude",
				"--api-key", "mir_srv_test",
				"--project", "770e8400-e29b-41d4-a716-446655440000",
				"--yes",
			}
			if tc.identity != "" {
				args = append(args, "--identity", tc.identity)
			}
			if _, err := runTerma(t, args...); err != nil {
				t.Fatalf("connect: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			for _, forbidden := range []string{"OTEL_RESOURCE_ATTRIBUTES", "enduser.id", "mirador.project.id"} {
				if strings.Contains(string(data), forbidden) {
					t.Errorf("%q reached Claude Code's settings:\n%s", forbidden, data)
				}
			}
		})
	}
}

// Disconnect must clean up a config whose telemetry was switched off but whose key is
// still on disk — the state where walking away is worst.
func TestTelemetryDisconnectCleansPartiallyDisabledConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	// Telemetry off, endpoint gone, key still present.
	seed := `{"env":{
		"CLAUDE_CODE_ENABLE_TELEMETRY":"0",
		"OTEL_EXPORTER_OTLP_HEADERS":"Authorization=Bearer mir_srv_leftover",
		"EDITOR":"vim"
	}}`
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := runTerma(t, "telemetry", "disconnect", "claude", "--yes"); err != nil {
		t.Fatalf("disconnect: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "mir_srv_leftover") {
		t.Fatalf("the server key survived disconnect:\n%s", data)
	}
	if !strings.Contains(string(data), "vim") {
		t.Errorf("disconnect removed an unrelated setting:\n%s", data)
	}
}

// The success message hands the user a command to run, so it has to be a command that
// works. The trace filter grammar accepts status, severity, tag and attribute.<key>; a
// bare `service.name` is rejected as an undeclared identifier, which is what the first
// version of this message printed.
// Connect used to end with `terma trace list --filter …`, a command of the CLI this one
// was forked from. The closing hint has to name something that runs here.
func TestTelemetryConnectEndsOnACommandThatExists(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	out, _, err := termaRun{}.exec(t,
		"telemetry", "connect", "claude",
		"--api-key", "mir_srv_test",
		"--project", "770e8400-e29b-41d4-a716-446655440000",
		"--yes",
	)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	got := out
	if !strings.Contains(got, "`terma session list`") {
		t.Errorf("connect did not say where the sessions show up:\n%s", got)
	}
	if strings.Contains(got, "terma trace") {
		t.Errorf("connect still names the trace command, which terma does not have:\n%s", got)
	}
}

// A bare connect captures everything: all three signals, prompt text, and tool
// content. The exclusion flags are the redaction path, and each turns off only its
// own capture.
func TestTelemetryConnectCapturesContentByDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want map[string]string
	}{
		{
			name: "bare connect",
			want: map[string]string{
				"OTEL_LOG_USER_PROMPTS":        "1",
				"OTEL_LOG_ASSISTANT_RESPONSES": "1",
				"OTEL_LOG_TOOL_DETAILS":        "1",
				"OTEL_LOG_TOOL_CONTENT":        "1",
				"OTEL_TRACES_EXPORTER":         "otlp",
				"OTEL_LOGS_EXPORTER":           "otlp",
				"OTEL_METRICS_EXPORTER":        "otlp",
			},
		},
		{
			name: "exclude prompts",
			args: []string{"--exclude-prompts"},
			want: map[string]string{
				"OTEL_LOG_USER_PROMPTS":        "0",
				"OTEL_LOG_ASSISTANT_RESPONSES": "0",
				"OTEL_LOG_TOOL_DETAILS":        "1",
				"OTEL_LOG_TOOL_CONTENT":        "1",
			},
		},
		{
			name: "exclude tool content",
			args: []string{"--exclude-tool-content"},
			want: map[string]string{
				"OTEL_LOG_USER_PROMPTS":        "1",
				"OTEL_LOG_ASSISTANT_RESPONSES": "1",
				"OTEL_LOG_TOOL_DETAILS":        "0",
				"OTEL_LOG_TOOL_CONTENT":        "0",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

			if _, err := runTerma(t, append([]string{
				"telemetry", "connect", "claude",
				"--api-key", "mir_srv_test",
				"--project", "770e8400-e29b-41d4-a716-446655440000",
				"--yes",
			}, tc.args...)...); err != nil {
				t.Fatalf("connect: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var doc struct {
				Env map[string]string `json:"env"`
			}
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatalf("parse: %v", err)
			}
			for key, want := range tc.want {
				if got := doc.Env[key]; got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

// Reconnecting the same project must reuse the installed key rather than minting an
// orphan. The proof is structural: the second connect has no --api-key and no stored
// login, so if it tried to mint it would fail with "not logged in" — succeeding at all
// means the key came from the existing config.
func TestTelemetryReconnectReusesInstalledKey(t *testing.T) {
	// Asserts production defaults, so it must not inherit the developer's TERMA_ENV:
	// the suite is run against dev, and this failed there for no reason of its own.
	t.Setenv("TERMA_ENV", "")
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	connect := func(args ...string) (string, error) {
		out, _, err := termaRun{}.exec(t, append([]string{
			"telemetry", "connect", "claude",
			"--project", "770e8400-e29b-41d4-a716-446655440000",
			"--yes",
		}, args...)...)
		return out, err
	}

	if _, err := connect("--api-key", "mir_srv_0123456789abcdef"); err != nil {
		t.Fatalf("first connect: %v", err)
	}

	// Same project, no key supplied, no login available — and different capture flags,
	// because tweaking settings is exactly when accidental re-minting used to happen.
	out, err := connect("--exclude-prompts")
	if err != nil {
		t.Fatalf("reconnect tried to mint instead of reusing: %v", err)
	}
	if !strings.Contains(out, "Reusing the key already configured") {
		t.Errorf("reconnect did not report the reuse:\n%s", out)
	}

	h := harness.Claude{}
	key, ok := h.CurrentCredential("https://otel.terma.ai", "770e8400-e29b-41d4-a716-446655440000")
	if !ok || key != "mir_srv_0123456789abcdef" {
		t.Fatalf("installed key after reconnect = (%q, %v), want the original", key, ok)
	}
}

// --inline-key opts out of the helper: the key goes into the settings file, which is
// then tightened, and no helper setting appears.
func TestTelemetryInlineKeyFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	if _, err := runTerma(t,
		"telemetry", "connect", "claude",
		"--api-key", "mir_srv_inline123",
		"--project", "770e8400-e29b-41d4-a716-446655440000",
		"--inline-key", "--yes",
	); err != nil {
		t.Fatalf("connect: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "mir_srv_inline123") {
		t.Fatal("--inline-key did not write the key into the settings file")
	}
	if strings.Contains(string(data), "otelHeadersHelper") {
		t.Fatal("--inline-key still installed a headers helper")
	}
}

// The default connect delivers the key through the helper: settings carry a path, the
// key does not appear in them at all.
func TestTelemetryDefaultConnectUsesHelper(t *testing.T) {
	dir := t.TempDir()
	miradorHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TERMA_CONFIG_DIR", miradorHome)

	if _, err := runTerma(t,
		"telemetry", "connect", "claude",
		"--api-key", "mir_srv_helperdefault1",
		"--project", "770e8400-e29b-41d4-a716-446655440000",
		"--yes",
	); err != nil {
		t.Fatalf("connect: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "mir_srv_helperdefault1") {
		t.Fatalf("the key landed in settings.json on a default connect:\n%s", data)
	}
	if !strings.Contains(string(data), "otelHeadersHelper") {
		t.Fatalf("no helper was configured on a default connect:\n%s", data)
	}
	helper := filepath.Join(miradorHome, "helpers",
		"claude-otel-770e8400-e29b-41d4-a716-446655440000")
	script, err := os.ReadFile(helper)
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	if !strings.Contains(string(script), "mir_srv_helperdefault1") {
		t.Fatal("the helper script does not hold the key")
	}
}

// Keys are per harness: a second harness does not borrow the key the first holds.
// Structural proof — the second connect has no --api-key and no login, so the only way
// it can end without an error is by taking Codex's key, which it must not.
func TestTelemetryConnectKeepsKeysSeparatePerHarness(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	const project = "770e8400-e29b-41d4-a716-446655440000"

	connect := func(args ...string) error {
		_, err := runTerma(t, append([]string{"telemetry", "connect"}, args...)...)
		return err
	}

	if err := connect("codex", "--project", project, "--yes", "--api-key", "mir_srv_0123456789abcdef"); err != nil {
		t.Fatalf("connect codex: %v", err)
	}
	if err := connect("claude", "--project", project, "--yes"); err == nil {
		t.Fatal("connect claude succeeded with no key and no login — it must have taken Codex's key")
	}
	h := harness.Claude{}
	if key, ok := h.CurrentCredential("https://otel.terma.ai", project); ok {
		t.Fatalf("Claude Code holds %q, want no key installed", harness.MaskKey(key))
	}
}

// Several harnesses in one command: each is connected with the key supplied.
func TestTelemetryConnectSeveralHarnessesAtOnce(t *testing.T) {
	// Asserts production defaults, so it must not inherit the developer's TERMA_ENV:
	// the suite is run against dev, and this failed there for no reason of its own.
	t.Setenv("TERMA_ENV", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	const project = "770e8400-e29b-41d4-a716-446655440000"

	out, _, err := termaRun{}.exec(t, "telemetry", "connect", "claude", "codex", "--project", project, "--yes", "--api-key", "mir_srv_0123456789abcdef")
	if err != nil {
		t.Fatalf("connect claude codex: %v", err)
	}
	for _, h := range []harness.Harness{harness.Claude{}, harness.Codex{}} {
		cur := h.(interface {
			CurrentCredential(endpoint, projectID string) (string, bool)
		})
		if key, ok := cur.CurrentCredential("https://otel.terma.ai", project); !ok || key != "mir_srv_0123456789abcdef" {
			t.Errorf("%s key = (%q, %v), want the supplied key", h.DisplayName(), key, ok)
		}
	}
	for _, section := range []string{"— claude —", "— codex —"} {
		if !strings.Contains(out, section) {
			t.Errorf("output lacks the %q section:\n%s", section, out)
		}
	}
}

// A typo anywhere in the list is refused before any harness is touched.
func TestTelemetryConnectRefusesListWithUnknownHarness(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	if _, err := runTerma(t, "telemetry", "connect", "claude", "gemini", "--project", "770e8400-e29b-41d4-a716-446655440000", "--yes", "--api-key", "mir_srv_0123456789abcdef"); err == nil || !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("err = %v, want a refusal naming gemini", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); !os.IsNotExist(err) {
		t.Errorf("Claude Code settings were written before the list was validated (stat err = %v)", err)
	}
}
