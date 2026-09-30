package builtin

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

func TestTargetsNormalizeSurfacesAndPreserveOrder(t *testing.T) {
	got := reg.RelayTargets([]string{"codex-desktop", "claude", "codex", "cursor", "pi", "pi"})
	want := []string{"claude", "codex", "pi"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	if reg.NameForTool("claude-code") != "claude" || reg.NameForTool("codex") != "codex" || reg.NameForTool("cursor") != "cursor" {
		t.Fatal("hook labels lost their key identity")
	}
	if _, ok := reg.Find[agents.RelayExporter]("cursor"); ok {
		t.Fatal("a hooks-only agent was accepted as an exporter")
	}
}

// Configuring one agent must neither overwrite another agent's files nor set the
// relay's credentials in the environment inherited by tools.
func TestConfigureIsolatesAgentFilesAndEnvironment(t *testing.T) {
	for _, e := range reg.With[agents.RelayExporter]() {
		t.Run(e.Name(), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SHELL", "/bin/sh")
			t.Setenv("PATH", "") // Hermes activation becomes a developer action.
			t.Setenv("TERMA_CONFIG_DIR", filepath.Join(home, "terma"))
			for key, dir := range map[string]string{
				"CLAUDE_CONFIG_DIR": "claude", "CODEX_HOME": "codex", "XDG_CONFIG_HOME": "xdg",
				"PI_CODING_AGENT_DIR": "pi", "OMP_DIR": "omp", "HERMES_HOME": "hermes",
				"GEMINI_CLI_HOME": "gemini", "DSH_HOME": "dsh",
			} {
				t.Setenv(key, filepath.Join(home, dir))
			}
			paths := map[string]string{
				"claude": "claude/settings.json", "codex": "codex/config.toml",
				"opencode": "xdg/opencode/plugins/terma.js", "omp": "omp/agent/extensions/terma-relay.ts",
				"pi": "pi/extensions/terma.ts", "hermes": "hermes/plugins/terma/__init__.py",
				"gemini": "gemini/.gemini/settings.json", "dsh": "dsh/plugins/terma-relay.mjs",
			}
			for name, path := range paths {
				if name == e.Name() {
					continue
				}
				full := filepath.Join(home, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte("other agent's configuration"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := os.Environ()
			state := filepath.Join(home, "state")
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			result, err := e.ConfigureRelay(t.Context(), agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "private-relay-token", HookCommand: []string{"/usr/local/bin/terma", "hook"}, StateDir: state})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, os.Environ()) {
				t.Fatal("configuration changed the tool environment")
			}
			for name, path := range paths {
				body, err := os.ReadFile(filepath.Join(home, path))
				if err != nil {
					t.Fatal(err)
				}
				if name != e.Name() && string(body) != "other agent's configuration" {
					t.Errorf("overwrote %s's configuration", name)
				}
				if name == e.Name() && !strings.Contains(string(body), "private-relay-token") {
					t.Error("the selected exporter lacks its local credential")
				}
			}
			if strings.Contains(strings.Join(result.Notes, " "), "private-relay-token") {
				t.Fatal("a configuration note exposed the local credential")
			}
			if e.Name() == "hermes" && (!result.Pending || len(result.Notes) == 0) {
				t.Fatal("failed activation was reported as ready")
			}
		})
	}
}
