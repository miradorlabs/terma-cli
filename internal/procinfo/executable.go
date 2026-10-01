package procinfo

import (
	"os"
	"path/filepath"
)

// AbsExecutable is the absolute path terma was started as, not its resolved target,
// which a package manager's upgrade removes.
func AbsExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}
