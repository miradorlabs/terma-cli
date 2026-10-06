package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// setup says where the credentials went: the keychain; with --insecure-storage, the file;
// and the file, flagged, where no keychain could be used. Running it again moves them.
func TestSetupSaysWhereTheCredentialsAre(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	tokens := func() bool {
		data, err := os.ReadFile(config.CredentialsPath(testApp.dir))
		return err == nil && strings.Contains(string(data), "ter_cli_")
	}

	out, err := runTerma(t, "setup", "--harness", "codex", "--insecure-storage")
	if err != nil || !strings.Contains(out, "Credentials   in plain text in") || !strings.Contains(out, "(--insecure-storage)") || !tokens() {
		t.Fatalf("--insecure-storage: %v, tokens in the file %v\n%s", err, tokens(), out)
	}
	out, err = runTerma(t, "setup", "--harness", "codex")
	if err != nil || !strings.Contains(out, "Credentials   in the system keychain") || tokens() {
		t.Fatalf("setup must move the tokens back to the keychain: %v, tokens in the file %v\n%s", err, tokens(), out)
	}
	if file, _ := config.LoadFile(testApp.dir); file.InsecureStorage {
		t.Fatal("insecure_storage outlived a setup without the flag")
	}
}

func TestSetupWithoutAKeychainFallsBackToTheFile(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	secret.FailForTest(t, testApp.dir)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--harness", "codex")
	if err != nil || !strings.Contains(out, "! Credentials   in plain text in") || !strings.Contains(out, "no system keychain could be used") {
		t.Fatalf("setup must flag the plain-text fallback: %v\n%s", err, out)
	}
	// Recorded, so the relay's later writes go straight to the file.
	if file, _ := config.LoadFile(testApp.dir); !file.InsecureStorage {
		t.Fatal("setup did not record the plain-text fallback")
	}
}

// A team key that fell back to keys.json moves into the keychain at the next setup that
// can use it, as the sign-in does.
func TestSetupMovesTeamKeysToTheKeychain(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	unlock := secret.FailForTest(t, testApp.dir)
	if err := keystore.Set(testApp.dir, "earlier-team", "ter_srv_0123456789abcdef", keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	unlock()
	if out, err := runTerma(t, "setup", "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(testApp.dir, "keys.json"))
	if err != nil || strings.Contains(string(data), "ter_srv_") {
		t.Fatalf("a team key stayed in plain text after setup: %v\n%s", err, data)
	}
	if key, err := keystore.Get(testApp.dir, "earlier-team"); err != nil || key != "ter_srv_0123456789abcdef" {
		t.Fatalf("keystore.Get = %q, %v", key, err)
	}
}

// Signing out with the keychain locked still signs out, and says what it could not do.
func TestSignOutWithALockedKeychain(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	secret.FailForTest(t, testApp.dir)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := testApp.signOut(cmd)
	if err == nil || !strings.Contains(err.Error(), "signed out, but") || !strings.Contains(out.String(), "could not read the sessions to revoke") {
		t.Fatalf("sign out = %v\n%s", err, &out)
	}
	if creds, err := auth.Credentials(testApp.dir, config.DefaultProfile); err != nil || len(creds) != 0 {
		t.Fatalf("the credential outlived sign-out: %+v, %v", creds, err)
	}
}

// A setup refused for a bad flag leaves the storage choice as it was.
func TestARejectedSetupKeepsTheStorageChoice(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	if err := config.UpdateFile(testApp.dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "setup", "--relay-service", "sometimes"); err == nil {
		t.Fatalf("--relay-service sometimes was accepted:\n%s", out)
	}
	if file, _ := config.LoadFile(testApp.dir); !file.InsecureStorage {
		t.Fatal("a rejected setup changed where secrets are kept")
	}
}

// A setup whose sign-in fails leaves the storage choice as it was.
func TestAFailedSignInKeepsTheStorageChoice(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	// Nobody completes the browser sign-in, so it fails when the context ends.
	if err := testApp.runSetup(cmd, setupFlags{harnesses: "codex", noBrowser: true, assumeYes: true, insecureStorage: true}); err == nil {
		t.Fatalf("setup with nobody to sign in succeeded:\n%s", &out)
	}
	if file, _ := config.LoadFile(testApp.dir); file.InsecureStorage {
		t.Fatal("a setup whose sign-in failed changed where secrets are kept")
	}
}
