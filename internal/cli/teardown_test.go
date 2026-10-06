package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

func TestTeardownRequiresConfirmationWithoutATerminal(t *testing.T) {
	_, _, stateDir, _ := nateTestEnvironment(t)
	relayDir := filepath.Join(stateDir, claim.DirName)
	if err := os.MkdirAll(relayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "teardown")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("teardown without confirmation = %v, output %q", err, out)
	}
	if _, err := os.Stat(relayDir); err != nil {
		t.Fatalf("teardown changed state before confirmation: %v", err)
	}
}

// Teardown undoes setup and nothing more: the relay's state and the hooks' go, so no hook
// starts the relay again, while the sign-in and the repository's binding stay.
func TestTeardownStopsTheRelayAndKeepsTheSignIn(t *testing.T) {
	_, configDir, stateDir, workspace := nateTestEnvironment(t)
	relayDir := filepath.Join(stateDir, claim.DirName)
	if err := os.MkdirAll(relayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relayDir, "token"), []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(configDir, "credentials.json")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// What hooks kept, which only hooks would prune, and the policy they ran under.
	hookState := []string{
		filepath.Join(stateDir, termaproject.WorkspacesDir, "h", "store.lock"),
		filepath.Join(stateDir, spool.Dir, "events.jsonl"),
		filepath.Join(stateDir, config.PoliciesDir, "team.json"), filepath.Join(stateDir, config.PoliciesDir, "team.json.lock"),
	}
	for _, dir := range testApp.hookStateDirs() {
		hookState = append(hookState, filepath.Join(stateDir, dir, "a.json"))
	}
	for _, path := range hookState {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The folder setup's records leave once each is restored.
	setupDir := filepath.Join(configDir, config.SetupDir)
	if err := os.MkdirAll(setupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binding := filepath.Join(workspace, ".terma", "settings.json")
	if err := os.MkdirAll(filepath.Dir(binding), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binding, []byte(`{"version":1,"project":{"id":"p"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	for range 2 { // safe to run again
		out, err := runTerma(t, "teardown", "--yes")
		if err != nil {
			t.Fatalf("teardown: %v\n%s", err, out)
		}
		if !strings.Contains(out, "`terma setup` sets it up again") {
			t.Fatalf("teardown should say how to undo it:\n%s", out)
		}
	}
	if _, err := os.Stat(relayDir); !os.IsNotExist(err) {
		t.Errorf("the relay's state survived teardown: %v", err)
	}
	if claim.Enabled(testApp.stateDir) {
		t.Error("hooks would still claim sessions for the relay")
	}
	for _, path := range hookState {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived teardown: %v", path, err)
		}
	}
	if _, err := os.Stat(setupDir); !os.IsNotExist(err) {
		t.Errorf("teardown left the emptied %s: %v", setupDir, err)
	}
	for _, kept := range []string{credentials, binding} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("teardown removed %s: %v", kept, err)
		}
	}
}

// A restoration that fails stops teardown before the relay's state goes.
func TestTeardownStopsWhenAnAgentCannotBeRestored(t *testing.T) {
	home, _, stateDir, _ := nateTestEnvironment(t)
	relayDir := filepath.Join(stateDir, claim.DirName)
	if err := os.MkdirAll(relayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeSettings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudeSettings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeSettings, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "teardown", "--yes")
	if err == nil || !strings.Contains(err.Error(), "restore Claude Code") {
		t.Fatalf("teardown with unreadable agent settings = %v, output %q", err, out)
	}
	if _, err := os.Stat(relayDir); err != nil {
		t.Fatalf("the relay's state went although restoration failed: %v", err)
	}
}

// --sign-out with an API key has no session to revoke, so teardown refuses before it changes anything.
func TestTeardownSignOutWithAnAPIKeyChangesNothing(t *testing.T) {
	_, _, stateDir, _ := nateTestEnvironment(t)
	relayDir := filepath.Join(stateDir, claim.DirName)
	if err := os.MkdirAll(relayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMA_API_KEY", "ter_srv_0123456789abcdef")
	out, err := runTerma(t, "teardown", "--sign-out", "--yes")
	if err == nil || !strings.Contains(err.Error(), "TERMA_API_KEY") {
		t.Fatalf("teardown --sign-out with an API key = %v, output %q", err, out)
	}
	if _, err := os.Stat(relayDir); err != nil {
		t.Fatalf("teardown changed state before refusing: %v", err)
	}
}

// A teardown and setup under the same sign-in keep the relay's token, so an agent still
// running with it is not refused; a sign-out discards it, and the next setup mints anew.
func TestTeardownThenSetupKeepsTheTokenForTheSameSignIn(t *testing.T) {
	relaySandbox(t)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	seedPolicyLogin(t, cfg.AuthURL)
	addr := freeAddr(t)
	setUp := func() string {
		t.Helper()
		if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr, "--harness", "claude"); err != nil {
			t.Fatalf("relay setup: %v\n%s", err, out)
		}
		token, err := daemon.Token(testApp.stateDir)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	before := setUp()
	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	if claim.Enabled(testApp.stateDir) {
		t.Fatal("teardown left the relay enabled")
	}
	if after := setUp(); after != before {
		t.Fatalf("setup after teardown minted a new token: agents still running present %q", before)
	}

	if err := testApp.undoSetup(t.Context(), io.Discard, false); err != nil { // teardown --sign-out
		t.Fatal(err)
	}
	if after := setUp(); after == before {
		t.Fatal("the token survived a sign-out")
	}
}

// Teardown removes the plugins terma wrote as an agent's relay exporter, which no connect journal covers.
func TestTeardownRemovesTheRelayPlugins(t *testing.T) {
	relaySandbox(t)
	home := os.Getenv("HOME")
	t.Setenv("GEMINI_CLI_HOME", home)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", freeAddr(t), "--harness", "pi,dsh,gemini"); err != nil {
		t.Fatalf("relay setup: %v\n%s", err, out)
	}
	plugins := []string{
		filepath.Join(home, ".pi", "agent", "extensions", "terma.ts"),
		filepath.Join(home, ".dsh", "plugins", "terma-relay.mjs"),
		filepath.Join(home, ".gemini", "extensions", "terma"),
	}
	for _, p := range plugins {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("setup wrote no %s: %v", p, err)
		}
	}
	out, err := runTerma(t, "teardown", "--yes")
	if err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	for _, p := range plugins {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived teardown: %v", p, err)
		}
	}
	if settings, _ := os.ReadFile(filepath.Join(home, ".gemini", "settings.json")); strings.Contains(string(settings), "otlpEndpoint") {
		t.Errorf("Gemini CLI still exports to the relay:\n%s", settings)
	}
	if !strings.Contains(out, "Gemini CLI no longer sends to the local relay") {
		t.Errorf("teardown did not say what it removed:\n%s", out)
	}
}

// Teardown restores no exporter setting changed since setup, and names each one.
func TestTeardownNamesTheSettingsItLeft(t *testing.T) {
	relaySandbox(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", freeAddr(t), "--harness", "claude"); err != nil {
		t.Fatalf("relay setup: %v\n%s", err, out)
	}
	path, err := harnessOf(t, "claude").ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	// One setting changed and one deleted: neither is restored, and each is named.
	env := settings["env"].(map[string]any)
	env["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://collector.example:4318"
	delete(env, "OTEL_LOG_USER_PROMPTS")
	if data, err = json.Marshal(settings); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "teardown", "--yes")
	if err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_LOG_USER_PROMPTS"} {
		if want := "Did not restore " + key + " in " + output.TildePath(path) + ": it was changed after setup. Check it holds what you want."; !strings.Contains(out, want) {
			t.Errorf("teardown did not name %s:\n%s", key, out)
		}
	}
}
