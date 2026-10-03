package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

func TestTeardownRequiresConfirmationWithoutATerminal(t *testing.T) {
	_, configDir, _ := nateTestEnvironment(t)
	relayDir := filepath.Join(configDir, claim.DirName)
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

// Teardown undoes setup and nothing more: the relay's state goes, so no hook starts it
// again, while the sign-in and the repository's binding stay.
func TestTeardownStopsTheRelayAndKeepsTheSignIn(t *testing.T) {
	_, configDir, workspace := nateTestEnvironment(t)
	relayDir := filepath.Join(configDir, claim.DirName)
	if err := os.MkdirAll(relayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relayDir, "token"), []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(configDir, "credentials.json")
	if err := os.WriteFile(credentials, []byte("{}"), 0o600); err != nil {
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
	if claim.Enabled() {
		t.Error("hooks would still claim sessions for the relay")
	}
	for _, kept := range []string{credentials, binding} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("teardown removed %s: %v", kept, err)
		}
	}
}

// A restoration that fails stops teardown before the relay's state goes.
func TestTeardownStopsWhenAnAgentCannotBeRestored(t *testing.T) {
	home, configDir, _ := nateTestEnvironment(t)
	relayDir := filepath.Join(configDir, claim.DirName)
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
	if err == nil || !strings.Contains(err.Error(), "restore Claude Code settings") {
		t.Fatalf("teardown with unreadable agent settings = %v, output %q", err, out)
	}
	if _, err := os.Stat(relayDir); err != nil {
		t.Fatalf("the relay's state went although restoration failed: %v", err)
	}
}

// --sign-out with an API key has no session to revoke, so teardown refuses before it changes anything.
func TestTeardownSignOutWithAnAPIKeyChangesNothing(t *testing.T) {
	_, configDir, _ := nateTestEnvironment(t)
	relayDir := filepath.Join(configDir, claim.DirName)
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
		token, err := daemon.Token()
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	before := setUp()
	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	if claim.Enabled() {
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
