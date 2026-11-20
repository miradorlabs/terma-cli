package cli

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// The relay and the spool's delivery send as the sign-in in place, read again at each send:
// its own account gets its token; another account, a signed-out profile, a retired sign-in
// and a server-key profile get none, which holds what waits for them.
func TestTheSignInSourceSendsOnlyAsItsOwnAccount(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	src := testApp.signInSource(cfg)
	if src.Account() != "" {
		t.Fatal("a signed-out profile has an account")
	}
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	src = testApp.signInSource(cfg)
	mine := auth.AccountKey(f.srv.URL, "u-1")
	if got := src.Account(); got != mine {
		t.Fatalf("account = %q, want u-1's", got)
	}
	if token, err := src.Token(t.Context(), mine); err != nil || token != fakeUserToken("u-1") {
		t.Fatalf("token = %q, %v", token, err)
	}
	if token, err := src.Token(t.Context(), auth.AccountKey(f.srv.URL, "u-2")); err != nil || token != "" {
		t.Fatalf("another account's token = %q, %v", token, err)
	}
	if err := auth.MarkRetired(testApp.dir, config.DefaultProfile, ""); err != nil {
		t.Fatal(err)
	}
	if token, err := src.Token(t.Context(), mine); err != nil || token != "" {
		t.Fatalf("a retired sign-in's token = %q, %v", token, err)
	}
	serverKeyProfile(t)
	if got := testApp.signInSource(cfg).Account(); got != "" {
		t.Fatalf("a server-key profile has account %q", got)
	}
}
