package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// OpenCode's harness is a plugin file Terma owns outright: connect writes it with the
// key in the helper script, status reads it back, disconnect removes both.
func TestTelemetryConnectOpenCodeInstallsThePlugin(t *testing.T) {
	xdg := t.TempDir()
	termaDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("TERMA_CONFIG_DIR", termaDir)

	out, err := runTerma(t, "connect", "opencode",
		"--api-key", "ter_srv_0123456789abcdef", "--project", "770e8400-e29b-41d4-a716-446655440000",
		"--yes", "--exclude-tool-content")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	plugin := filepath.Join(xdg, "opencode", "plugins", "terma.js")
	helper := filepath.Join(termaDir, "helpers", "opencode-otel-770e8400-e29b-41d4-a716-446655440000")
	for _, want := range []string{plugin, helper, "restart it after connecting", "`terma session list`", "no separate metrics stream"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	data, err := os.ReadFile(plugin)
	if err != nil {
		t.Fatalf("plugin not written: %v", err)
	}
	if !strings.Contains(string(data), "const CONFIG = {") || strings.Contains(string(data), "ter_srv_0123456789abcdef") {
		t.Fatalf("plugin config not spliced, or the key leaked into it")
	}
	if _, err := os.Stat(helper); err != nil {
		t.Fatalf("helper not written: %v", err)
	}
	// Nothing of OpenCode's own configuration was created.
	if _, err := os.Stat(filepath.Join(xdg, "opencode", "opencode.json")); err == nil {
		t.Fatal("connect wrote opencode.json")
	}

	out, err = runTerma(t, "harness", "status", "opencode", "-o", "json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var report telemetryStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if len(report.Harnesses) != 1 {
		t.Fatalf("got %d entries:\n%s", len(report.Harnesses), out)
	}
	st := report.Harnesses[0]
	if st.State != "connected" || st.Signals != "logs,metrics,traces" || st.Prompts != "on" || st.ToolContent != "off" {
		t.Errorf("status = %+v", st)
	}
	if st.KeyPrefix == "" || strings.Contains(out, "ter_srv_0123456789abcdef") {
		t.Errorf("key must be reported masked, never whole:\n%s", out)
	}
	if st.ProjectID != "770e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("project = %q", st.ProjectID)
	}

	// Reconnecting reuses the installed key rather than needing one: no --api-key, no
	// login, and it still succeeds.
	if out, err := runTerma(t, "connect", "opencode", "--project", "770e8400-e29b-41d4-a716-446655440000", "--yes"); err != nil {
		t.Fatalf("reconnect: %v\n%s", err, out)
	} else if !strings.Contains(out, "Reusing the key") {
		t.Errorf("reconnect minted instead of reusing:\n%s", out)
	}

	out, err = runTerma(t, "disconnect", "opencode", "--yes")
	if err != nil {
		t.Fatalf("disconnect: %v\n%s", err, out)
	}
	if _, err := os.Stat(plugin); err == nil {
		t.Error("plugin survived disconnect")
	}
	if _, err := os.Stat(helper); err == nil {
		t.Error("helper survived disconnect")
	}
}

// A repository policy for OpenCode is a committed file with no destination in it.
func TestTelemetryConnectOpenCodeLocalWritesPolicy(t *testing.T) {
	repo, _ := localRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	out, err := runTerma(t, "connect", "opencode", "--scope", "local", "--yes", "--signals", "traces", "--exclude-prompts")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".opencode", "terma.json"))
	if err != nil {
		t.Fatalf("policy not written: %v", err)
	}
	var policy struct {
		Signals            []string `json:"signals"`
		IncludePrompts     bool     `json:"includePrompts"`
		IncludeToolContent bool     `json:"includeToolContent"`
	}
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Signals) != 1 || policy.Signals[0] != "traces" || policy.IncludePrompts || !policy.IncludeToolContent {
		t.Errorf("policy = %+v", policy)
	}
	if strings.Contains(string(data), "endpoint") || strings.Contains(string(data), "headers") {
		t.Errorf("policy carries a destination:\n%s", data)
	}
	out, err = runTerma(t, "harness", "status", "opencode", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"scope": "local"`) || !strings.Contains(out, "local settings, not exporting") {
		t.Errorf("status lacks the local row:\n%s", out)
	}
}
