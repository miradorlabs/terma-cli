package procinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// AbsExecutable is the absolute path terma was started as, not its resolved target, which
// a package manager's upgrade removes: a symlink Homebrew repoints names the new build,
// where Linux's os.Executable names the old one's keg.
func AbsExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if started := startedAs(); started != "" && sameFile(started, exe) {
		return started, nil
	}
	return filepath.Abs(exe)
}

// startedAs is os.Args[0] as an absolute path, looked up on PATH when it is a bare name.
func startedAs() string {
	arg := os.Args[0]
	if !strings.ContainsAny(arg, `/\`) {
		var err error
		if arg, err = exec.LookPath(arg); err != nil {
			return ""
		}
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return ""
	}
	return abs
}

func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}
