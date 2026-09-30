package procinfo

import (
	"os"
	"path/filepath"
)

// AbsExecutable is the path this terma was started as, made absolute, not its resolved
// target: a package manager's upgrade removes the old target, and whatever runs terma
// by this path (a service, a machine-wide hook) must start what the path names next.
func AbsExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}
