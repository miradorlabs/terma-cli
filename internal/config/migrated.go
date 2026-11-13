package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// The machine-wide indexes the migration to multi-org routing moves keys and sign-ins
// into. Older binaries read only keys.json and credentials.json, so once those are
// retired an older terma finds no key to send with and no sign-in to fetch a policy.
const (
	keysV2FileName        = "keys.v2.json"
	credentialsV2FileName = "credentials.v2.json"
)

// KeysV2Path is where keys.v2.json, the index of each enrollment's team key, lives under
// the config directory dir.
func KeysV2Path(dir string) string { return filepath.Join(dir, keysV2FileName) }

// CredentialsV2Path is where credentials.v2.json, the sign-ins of a migrated machine,
// lives under the config directory dir.
func CredentialsV2Path(dir string) string { return filepath.Join(dir, credentialsV2FileName) }

// Migrated reports whether the machine whose config directory is dir has been migrated:
// whether credentials.v2.json exists. That file is the one marker, kept in the config
// directory so that it outlives a lost routing state, which then fails closed instead of
// looking like a fresh setup. A file that cannot be looked at for any reason but its
// absence counts as there.
func Migrated(dir string) bool {
	_, err := os.Lstat(CredentialsV2Path(dir))
	return !errors.Is(err, fs.ErrNotExist)
}
