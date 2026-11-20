//go:build unix

package cli

import (
	"os"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
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
