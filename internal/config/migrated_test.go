package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The v2 indexes sit beside the v1 files, and credentials.v2.json alone marks a migrated
// machine.
func TestMigrated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if KeysV2Path(dir) != filepath.Join(dir, "keys.v2.json") || CredentialsV2Path(dir) != filepath.Join(dir, "credentials.v2.json") {
		t.Fatalf("v2 paths: %s, %s", KeysV2Path(dir), CredentialsV2Path(dir))
	}
	if Migrated(dir) {
		t.Fatal("an empty config directory is migrated")
	}
	for _, other := range []string{"keys.v2.json", "credentials.json", "keys.json"} {
		if err := os.WriteFile(filepath.Join(dir, other), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if Migrated(dir) {
		t.Fatal("a file other than credentials.v2.json marked the machine migrated")
	}
	if err := os.WriteFile(CredentialsV2Path(dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !Migrated(dir) {
		t.Fatal("credentials.v2.json did not mark the machine migrated")
	}
	if Migrated(filepath.Join(dir, "missing")) {
		t.Fatal("a config directory that does not exist is migrated")
	}
}
