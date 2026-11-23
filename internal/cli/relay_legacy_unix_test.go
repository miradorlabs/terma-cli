//go:build unix

package cli

import (
	"os"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// A server key's look that can't be written is no decision, and is tried again until it
// lands.
func TestAnUnwrittenLookIsNoDecisionUntilItLands(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes where a mode forbids")
	}
	authSandbox(t, newFakeAuth(t))
	serverKeyProfile(t)
	stateDir := testApp.stateDir
	data := `{"default": {"organizations": {"o": {"session_id": "s", "auth_url": "https://auth.example.test", "user_id": "u"}}}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })
	legacy := func() auth.LegacyRecord { return testApp.legacyRecord(config.DefaultProfile) }
	if first := legacy(); first.Account != "" {
		t.Fatalf("first look = %+v; want no decision", first)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := legacy(); got.Account != auth.LegacyNoPerson {
		t.Fatalf("look = %+v; want no person's", got)
	}
	if rec, err := auth.ReadLegacyRecord(stateDir); err != nil || rec.Account != auth.LegacyNoPerson {
		t.Fatalf("record = %+v, %v; want the key's decision written", rec, err)
	}
}
