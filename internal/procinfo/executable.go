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

// Executable is the file a process runs.
type Executable struct {
	// Path is the file's path, for its name. On Linux it stays the path the process started
	// from after an update replaced or removed that file.
	Path string
	// Run reaches the file the process runs, to stat or start: Path, or on Linux
	// /proc/<pid>/exe once the file at Path is no longer the one running.
	Run string
}

// ExecutableOf is the file process pid runs, and whether that could be told: from
// /proc/<pid>/exe on Linux, from the kernel's proc_info call on macOS. macOS answers with the
// path alone, so after an update replaced the file there, Run is the new file. Windows and
// other systems cannot tell.
func ExecutableOf(pid int) (Executable, bool) {
	if pid <= 0 {
		return Executable{}, false
	}
	return executableOf(pid)
}
