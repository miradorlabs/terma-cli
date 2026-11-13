package migrate

import (
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// OptInFile is the state directory's record that the developer moved this machine to
// routing by teams, and when (OptIn): `terma teams` writes it, and so does `terma setup`
// on a machine never set up before. A relay start migrates a machine not yet migrated only
// once it is there, or when Machine.Automatic says so; one that is marked migrated already
// is finished either way. Once the machine is migrated it changes nothing: there is no
// going back.
const OptInFile = "migration-opted-in"

// OptIn records under stateDir that the developer moved this machine to routing by teams,
// so the next relay start migrates it. Opting in again keeps the first record.
func OptIn(stateDir string) error {
	if OptedIn(stateDir) {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(stateDir, OptInFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// OptedIn reports whether the developer moved the machine under stateDir to routing by
// teams (OptIn).
func OptedIn(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, OptInFile))
	return err == nil
}
