//go:build unix

package cli

import (
	"os"
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
