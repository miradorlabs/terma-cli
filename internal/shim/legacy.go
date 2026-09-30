// Package shim removes what earlier terma builds installed to route agents per
// repository — PATH shims (a directory of scripts ahead of the real claude and codex),
// the marked block in the shell's startup file that put them on PATH, and Claude Code's
// per-project settings documents — and keeps the scripts still installed on a machine
// working until they are gone. Agents are routed by the local relay now
// (docs/RELAY-SPIKE.md); nothing here installs anything.
//
// It also knows the developer's shell startup file (ShellRC), which doctor names when it
// tells a developer how to put terma's own directory on PATH.
package shim

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
)

func dir(parts ...string) (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{base}, parts...)...), nil
}

// ShimBinDir is the directory earlier builds put their PATH-shim scripts in.
func ShimBinDir() (string, error) { return dir("shim", "bin") }

// RemoveLegacy takes away every trace of the shims: the scripts, the block that put
// them on PATH, and Claude Code's per-project settings documents. The routing records
// stay — they are the relay's per-project policy — and so do the keys.
func RemoveLegacy() (removed bool, err error) {
	binDir, err := ShimBinDir()
	if err != nil {
		return false, err
	}
	if entries, err := os.ReadDir(binDir); err == nil {
		removed = len(entries) > 0
		if err := os.RemoveAll(filepath.Dir(binDir)); err != nil {
			return removed, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if rc, ok := ShellRC(); ok {
		did, err := rc.Remove()
		if err != nil {
			return removed, err
		}
		removed = removed || did
	}
	claude, err := dir("claude")
	if err != nil {
		return removed, err
	}
	if err := os.RemoveAll(claude); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return removed, err
	}
	return removed, nil
}

// PrepareNothing answers a legacy shim script's `terma shim prepare <agent> <dir> --
// args`: a plan with no arguments, so the script starts the real agent as it is,
// quietly. Without it the script would say "routing preparation failed" at every launch
// until the shims are removed.
func PrepareNothing(planDir string) error {
	info, err := os.Stat(planDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("no plan directory %q", planDir)
	}
	return os.WriteFile(filepath.Join(planDir, "count"), []byte("terma-args-v1:0\n"), 0o600)
}

// Exec answers the oldest shim scripts' `terma shim exec <agent> args`: the real agent,
// found on PATH past the shim directory, started with the arguments unchanged.
func Exec(agent string, args []string) error {
	binary, err := RealBinary(agent)
	if err != nil {
		return err
	}
	return execReal(binary, args, os.Environ())
}

// RealBinary finds the agent's binary on PATH, skipping the shim directory so a shim
// never re-invokes itself.
func RealBinary(agent string) (string, error) {
	shimDir, _ := ShimBinDir()
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == "" || (shimDir != "" && sameDir(d, shimDir)) {
			continue
		}
		candidate := filepath.Join(d, agent)
		// A "." entry joins to a bare name, which exec.LookPath would resolve against the
		// whole PATH again, the shim directory included.
		if !strings.ContainsRune(candidate, filepath.Separator) {
			candidate = "." + string(filepath.Separator) + candidate
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s is not installed (not on PATH)", agent)
}

// sameDir reports whether two paths name one directory: a PATH entry is whatever the
// developer typed (a trailing slash, a symlinked home), and missing the shim directory
// would have RealBinary hand back the shim itself.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}
