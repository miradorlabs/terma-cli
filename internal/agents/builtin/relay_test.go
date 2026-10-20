package builtin

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// Targets keep the selection's order once each, and drop a name that exports nothing.
func TestTargetsDropNonExportersAndPreserveOrder(t *testing.T) {
	t.Parallel()
	got := reg.RelayTargets([]string{"claude", "codex", "cursor", "pi", "pi", "unknown"})
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

// Configuring one agent neither overwrites another's files nor puts the relay's
// credentials in the environment tools inherit.
func TestConfigureIsolatesAgentFilesAndEnvironment(t *testing.T) {
	for _, a := range reg.With[agents.RelayExporter]() {
		t.Run(a.Name(), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SHELL", "/bin/sh")
			t.Setenv("PATH", "") // Hermes activation becomes a developer action.
			e, _ := Agents(filepath.Join(home, "terma")).Find[agents.RelayExporter](a.Name())
			for key, dir := range map[string]string{
				"CLAUDE_CONFIG_DIR": "claude", "CODEX_HOME": "codex", "XDG_CONFIG_HOME": "xdg",
				"PI_CODING_AGENT_DIR": "pi", "OMP_DIR": "omp", "HERMES_HOME": "hermes",
				"GEMINI_CLI_HOME": "gemini", "DSH_HOME": "dsh", "COPILOT_HOME": "copilot",
			} {
				t.Setenv(key, filepath.Join(home, dir))
			}
			paths := map[string]string{
				"claude": "claude/settings.json", "codex": "codex/config.toml",
				"opencode": "xdg/opencode/plugins/terma.js", "omp": "omp/agent/extensions/terma-relay.ts",
				"pi": "pi/extensions/terma.ts", "hermes": "hermes/plugins/terma/__init__.py",
				"gemini": "gemini/.gemini/settings.json", "dsh": "dsh/plugins/terma-relay.mjs",
				"copilot": vscodeSettings(t, home),
			}
			if _, ok := paths[e.Name()]; !ok {
				t.Fatalf("this test knows no configuration file for %s", e.Name())
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
			result, err := e.ConfigureRelay(t.Context(), agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "private-relay-token", HookCommand: []string{"/usr/local/bin/terma", "hook"}})
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
			for _, p := range result.Paths {
				if rel, err := filepath.Rel(home, p); err != nil || strings.HasPrefix(rel, "..") {
					t.Errorf("wrote %s, outside the developer's home", p)
					continue
				}
				if body, _ := os.ReadFile(p); strings.Contains(string(body), "private-relay-token") {
					if info, _ := os.Stat(p); info.Mode().Perm()&0o077 != 0 {
						t.Errorf("%s holds the relay's token and is %v", p, info.Mode().Perm())
					}
				}
			}
			if pointed, known := e.RelayPointed("127.0.0.1:43180"); known && !pointed {
				t.Error("the exporter it just configured does not read as pointed at the relay")
			}
			if e.Name() == "hermes" && (!result.Pending || len(result.Notes) == 0) {
				t.Fatal("failed activation was reported as ready")
			}
		})
	}
}

// vscodeSettings is the user settings file, relative to home, of a VS Code Copilot has run
// in, in the sandbox: an editor it never ran in gets no settings.
func vscodeSettings(t *testing.T, home string) string {
	t.Helper()
	base := filepath.Join(home, "vscode")
	t.Setenv("VSCODE_APPDATA", base)
	// No exporter of the developer's own in the shell the tests run from: VS Code would
	// read it ahead of its settings, and setup then writes none.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	user := filepath.Join(base, "Code", "User")
	if err := os.MkdirAll(filepath.Join(user, "globalStorage", "github.copilot-chat"), 0o700); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(home, filepath.Join(user, "settings.json"))
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("VS Code's settings are outside the sandbox: %s, %v", user, err)
	}
	return filepath.ToSlash(rel)
}

// Unconfiguring leaves none of the relay's token in an agent's files, and again has nothing to undo.
func TestUnconfigureRelayUndoesConfigure(t *testing.T) {
	for _, a := range reg.With[agents.RelayExporter]() {
		t.Run(a.Name(), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SHELL", "/bin/sh")
			t.Setenv("PATH", "") // no Hermes to enable or disable the plugin
			for key, dir := range map[string]string{
				"CLAUDE_CONFIG_DIR": "claude", "CODEX_HOME": "codex", "XDG_CONFIG_HOME": "xdg",
				"PI_CODING_AGENT_DIR": "pi", "OMP_DIR": "omp", "HERMES_HOME": "hermes",
				"GEMINI_CLI_HOME": "gemini", "DSH_HOME": "dsh", "COPILOT_HOME": "copilot",
			} {
				t.Setenv(key, filepath.Join(home, dir))
			}
			e, _ := Agents(filepath.Join(home, "terma")).Find[agents.RelayExporter](a.Name())
			vscodeSettings(t, home)
			const token = "relay-token-0123456789abcdef"
			cfg := agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: token, HookCommand: []string{"terma", "hook"}}
			if _, err := e.ConfigureRelay(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			holding := func() []string {
				var found []string
				_ = filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						return err
					}
					if data, _ := os.ReadFile(path); strings.Contains(string(data), token) {
						found = append(found, strings.TrimPrefix(path, home))
					}
					return nil
				})
				return found
			}
			if len(holding()) == 0 {
				t.Fatal("configure wrote the token nowhere; the test cannot tell an undo")
			}
			undone, err := e.UnconfigureRelay(t.Context(), token)
			if err != nil || len(undone.Paths) == 0 {
				t.Fatalf("unconfigure: %+v, %v", undone, err)
			}
			if left := holding(); len(left) > 0 {
				t.Fatalf("the relay's token is still in %v", left)
			}
			if again, err := e.UnconfigureRelay(t.Context(), token); err != nil || len(again.Paths) > 0 {
				t.Fatalf("a second unconfigure changed %v, %v", again.Paths, err)
			}
		})
	}
}

// A refresh rewrites a relay exporter's own file in this build's template, keeping its
// configuration and its mode, and writes none that is not there.
func TestRefreshRewritesRelayExporterFiles(t *testing.T) {
	paths := map[string]string{
		"opencode": "xdg/opencode/plugins/terma.js", "pi": "pi/extensions/terma.ts",
		"omp": "omp/agent/extensions/terma-relay.ts", "dsh": "dsh/plugins/terma-relay.mjs",
		"hermes": "hermes/plugins/terma/__init__.py",
	}
	for name, rel := range paths {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("PATH", "") // Hermes's activation is left to the developer.
			for key, dir := range map[string]string{
				"XDG_CONFIG_HOME": "xdg", "PI_CODING_AGENT_DIR": "pi", "OMP_DIR": "omp", "HERMES_HOME": "hermes", "DSH_HOME": "dsh",
			} {
				t.Setenv(key, filepath.Join(home, dir))
			}
			reg := Agents(filepath.Join(home, "terma"))
			e, _ := reg.Find[agents.RelayExporter](name)
			r, ok := reg.Find[agents.MachineRefresher](name)
			if !ok {
				t.Fatalf("%s's relay exporter file is not refreshed", name)
			}
			path := filepath.Join(home, rel)
			if _, changed, err := r.RefreshMachine(); err != nil || changed {
				t.Fatalf("a refresh with nothing written: changed=%v err=%v", changed, err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("a refresh wrote %s: %v", rel, err)
			}
			if _, err := e.ConfigureRelay(t.Context(), agents.RelayConfig{Endpoint: "http://127.0.0.1:43180", Token: "relay-token", HookCommand: []string{"terma", "hook"}}); err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// What an earlier build wrote differs from this one's template.
			if err := os.WriteFile(path, append(slices.Clone(want), '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, changed, err := r.RefreshMachine(); err != nil || !changed || got != path {
				t.Fatalf("refresh: path=%q changed=%v err=%v", got, changed, err)
			}
			if got, _ := os.ReadFile(path); string(got) != string(want) {
				t.Errorf("refreshed %s:\n%s\nwant:\n%s", rel, got, want)
			}
			if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Errorf("refreshed %s is %v: it holds the relay's token", rel, info.Mode().Perm())
			}
			if _, changed, err := r.RefreshMachine(); err != nil || changed {
				t.Fatalf("a second refresh: changed=%v err=%v", changed, err)
			}
		})
	}
}
