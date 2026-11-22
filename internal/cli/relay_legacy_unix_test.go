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

// A server key's first look that can't be written is tried again, with the same boundary,
// until it lands: the boundary the relay judged by is the one recorded.
func TestAnUnwrittenFirstLookIsWrittenLaterWithItsBoundary(t *testing.T) {
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
	first := legacy()
	if first.RetiredAt.IsZero() || first.Account != auth.LegacyAccountAmbiguous {
		t.Fatalf("first look = %+v", first)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if got := legacy(); !got.RetiredAt.Equal(first.RetiredAt) {
		t.Fatalf("boundary moved from %v to %v", first.RetiredAt, got.RetiredAt)
	}
	if rec, err := auth.ReadLegacyRecord(stateDir); err != nil || !rec.RetiredAt.Equal(first.RetiredAt) {
		t.Fatalf("record = %+v, %v; want the first look's boundary written", rec, err)
	}
}

// A record that can't be read is read again at the next look: once readable, though it
// looks unchanged, its own account and boundary count, not the unreadable stand-in.
func TestAnUnreadableLegacyRecordIsReadAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file its mode forbids")
	}
	dir, stateDir := t.TempDir(), t.TempDir()
	at := time.Now().Add(-time.Hour).UTC()
	if err := auth.RecordLegacy(stateDir, []auth.LegacySignIn{{SessionID: "s", AuthURL: "https://auth.example.test", UserID: "u"}}, at); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, auth.LegacyRecordFile)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	legacy := cachedLegacyRecord(dir, stateDir, func() (bool, error) { return true, nil })
	if got := legacy(); got.Account != auth.LegacyAccountAmbiguous {
		t.Fatalf("unreadable: %+v; want it named by no one", got)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	want := auth.AccountKey("https://auth.example.test", "u")
	if got := legacy(); got.Account != want || !got.RetiredAt.Equal(at) {
		t.Fatalf("readable again: %+v; want its own account %q and boundary %v", got, want, at)
	}
}

// A retirement that writes its record before an unwritten first look lands keeps the first
// look's boundary: what the server key collected between the two is never counted as the
// person's backlog.
func TestARetirementBeforeTheFirstLookLandsKeepsItsBoundary(t *testing.T) {
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
	first := legacy()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The person signs in and retires the older sign-ins, later, before the retry.
	later := first.RetiredAt.Add(time.Minute)
	if err := auth.RecordLegacy(stateDir, []auth.LegacySignIn{{SessionID: "s", AuthURL: "https://auth.example.test", UserID: "u"}}, later); err != nil {
		t.Fatal(err)
	}
	if got := legacy(); got.Account != auth.LegacyAccountAmbiguous || !got.RetiredAt.Equal(first.RetiredAt) {
		t.Fatalf("judged by %+v; want the first look's boundary, no one's", got)
	}
	if rec, err := auth.ReadLegacyRecord(stateDir); err != nil || !rec.RetiredAt.Equal(first.RetiredAt) || rec.Account != auth.LegacyAccountAmbiguous {
		t.Fatalf("record = %+v, %v; want the first look's boundary kept", rec, err)
	}
}
