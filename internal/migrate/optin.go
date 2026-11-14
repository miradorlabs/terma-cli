package migrate

import (
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// OptInFile records under the state directory when the developer opted in to routing by
// teams (OptIn): `terma teams` writes it, as does `terma setup` on a new machine. A relay
// start migrates only with it, or Machine.Automatic. Once migrated, it changes nothing.
const OptInFile = "migration-opted-in"

// OptIn records under stateDir that the developer opted in, so the next relay start
// migrates. Opting in again keeps the first record.
func OptIn(stateDir string) error {
	if OptedIn(stateDir) {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(stateDir, OptInFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// OptedIn reports whether the developer opted in under stateDir (OptIn).
func OptedIn(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, OptInFile))
	return err == nil
}
