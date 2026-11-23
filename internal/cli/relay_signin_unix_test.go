//go:build unix

package cli

import (
	"os"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// A sign-in refused as retired gives no token again, even when the store can't take the
// mark: the client that sent it holds it retired.
func TestARetiredSignInGivesNoTokenWhenTheStoreFails(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := config.UpdateFile(testApp.dir, func(file *config.File) { file.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	src := testApp.signInSource(cfg)
	mine := auth.AccountKey(f.srv.URL, "u-1")
	token, err := src.Token(t.Context(), mine)
	if err != nil || token == "" {
		t.Fatalf("token = %q, %v", token, err)
	}
	if err := os.Chmod(testApp.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(testApp.dir, 0o700) })
	src.Retired(token)
	if auth.Retired(testApp.dir, config.DefaultProfile) {
		t.Fatal("the read-only store took the mark; the test proves nothing")
	}
	if again, err := src.Token(t.Context(), mine); err != nil || again != "" {
		t.Fatalf("a retired sign-in's token = %q, %v; want none", again, err)
	}
}

// A profile whose mode can't be read binds nothing to the person signed in, nor to no one:
// it may be a server key's, which keeps an earlier sign-in on disk, or a person's. Once it
// can be read again, the sign-in counts again, though the file looks unchanged.
func TestAnUnreadableProfileBindsTheUnreadableAccount(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file its mode forbids")
	}
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateFile(testApp.dir, func(*config.File) {}); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config.Path(testApp.dir), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(config.Path(testApp.dir), 0o600) })
	src := testApp.signInSource(cfg)
	if got := src.Account(); got != sendas.AccountUnreadable {
		t.Fatalf("with the profile unreadable, account = %q; want %q", got, sendas.AccountUnreadable)
	}
	if err := os.Chmod(config.Path(testApp.dir), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := src.Account(); got != auth.AccountKey(f.srv.URL, "u-1") {
		t.Fatalf("readable again, account = %q; want u-1's", got)
	}
}

// A sign-in that can't be read binds what arrives to no sign-in that could send it, nor to
// no one: AccountUnreadable, read again at the next export, though the file looks unchanged.
func TestAnUnreadableSignInBindsTheUnreadableAccount(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file its mode forbids")
	}
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config.CredentialsPath(testApp.dir), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(config.CredentialsPath(testApp.dir), 0o600) })
	src := testApp.signInSource(cfg)
	if got := src.Account(); got != sendas.AccountUnreadable {
		t.Fatalf("with the sign-in unreadable, account = %q", got)
	}
	if err := os.Chmod(config.CredentialsPath(testApp.dir), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := src.Account(); got != auth.AccountKey(f.srv.URL, "u-1") {
		t.Fatalf("readable again, account = %q; want u-1's", got)
	}
}
