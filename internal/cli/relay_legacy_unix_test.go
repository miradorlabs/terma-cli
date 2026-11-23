//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// A server key's look that can't be written is no decision, and is tried again until it
// lands.
func TestAnUnwrittenLookIsNoDecisionUntilItLands(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes where a mode forbids")
	}
	dir, stateDir := t.TempDir(), t.TempDir()
	data := `{"default": {"organizations": {"o": {"session_id": "s", "auth_url": "https://auth.example.test", "user_id": "u"}}}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })
	legacy := cachedLegacyRecord(dir, stateDir, func() (bool, error) { return true, nil })
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

// A record that can't be read is read again at the next look: once readable, though it
// looks unchanged, its own decision counts.
func TestAnUnreadableLegacyRecordIsReadAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file its mode forbids")
	}
	dir, stateDir := t.TempDir(), t.TempDir()
	want := auth.AccountKey("https://auth.example.test", "u")
	if err := auth.RecordLegacy(stateDir, []auth.LegacySignIn{{SessionID: "s", AuthURL: "https://auth.example.test", UserID: "u"}}, want, time.Now()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, auth.LegacyRecordFile)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	legacy := cachedLegacyRecord(dir, stateDir, func() (bool, error) { return true, nil })
	if got := legacy(); got.Account != "" {
		t.Fatalf("unreadable: %+v; want no decision", got)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := legacy(); got.Account != want {
		t.Fatalf("readable again: %+v; want its own decision %q", got, want)
	}
}

// A retirement that writes its decision before a server key's unwritten look lands keeps
// it: the first decision written stands.
func TestARetirementBeforeTheLookLandsKeepsItsDecision(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes where a mode forbids")
	}
	dir, stateDir := t.TempDir(), t.TempDir()
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })
	legacy := cachedLegacyRecord(dir, stateDir, func() (bool, error) { return true, nil })
	_ = legacy()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := auth.AccountKey("https://auth.example.test", "u")
	if err := auth.RecordLegacy(stateDir, []auth.LegacySignIn{{SessionID: "s", AuthURL: "https://auth.example.test", UserID: "u"}}, want, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := legacy(); got.Account != want {
		t.Fatalf("decided %+v; want the retirement's", got)
	}
}
