package selfupdate

import (
	"path/filepath"
	"runtime"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Install is how a terma binary was installed, which decides how it gets new releases: what
// the relay reports on its heartbeat, and what its updater does.
type Install struct {
	// Exe is the binary, "" when the process could not find the one it started from.
	Exe string
	// Method is terma.install.method: semconv.TermaInstallMethodScript, Homebrew, Npm,
	// NpmProject or Source; "" for a release binary the process could not find, which
	// reports none rather than a guess.
	Method string
	// Manager is the package manager that owns a Homebrew or npm install.
	Manager Manager
	goos    string
	// writes are the directories an update writes in: the binary's own, for one terma
	// replaces in place, or its package manager's (Manager.writes).
	writes []string
}

// InstallOf tells how the terma at exe, at version, was installed, from its path and version
// alone. The version comes first: a build that is not a release (IsRelease) is a source
// build wherever it is, a package manager's directory included. An exe of "" is a binary
// the process could not find: a release's has no method, and is never updated (Automatic).
func InstallOf(exe, version string) Install {
	return InstallOn(exe, version, runtime.GOOS)
}

// InstallOn is InstallOf for a terma built for goos, as a test names another platform's.
func InstallOn(exe, version, goos string) Install {
	i := Install{Exe: exe, goos: goos}
	m, managed := managedBy(exe, goos)
	switch {
	case !IsRelease(version):
		i.Method = semconv.TermaInstallMethodSource
	case exe == "":
	case !managed:
		// The new binary is written beside the one it replaces, its link resolved.
		resolved := exe
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			resolved = r
		}
		i.Method, i.writes = semconv.TermaInstallMethodScript, []string{filepath.Dir(resolved)}
	case m.Name == "Homebrew":
		i.Method, i.Manager, i.writes = semconv.TermaInstallMethodHomebrew, m, m.writes
	case m.Project != "":
		i.Method, i.Manager = semconv.TermaInstallMethodNpmProject, m
	default:
		i.Method, i.Manager, i.writes = semconv.TermaInstallMethodNpm, m, m.writes
	}
	return i
}

// Automatic is terma.update.auto for this install, under the developer's choice p and the
// environment getenv reads: whether the relay installs each new release by itself
// (semconv.TermaUpdateAutoOn), could but is told not to (Off: `terma update --auto off`, or
// CI or TERMA_NO_UPDATE_CHECK=1, which keep terma's updates out of automation), or cannot,
// whatever the developer chooses (Unsupported): ever (CanUpdate), or now, where this process
// may not write where an update writes (updatable). It is the one rule: the relay's updater
// installs releases exactly when it says on, so what the heartbeat reports is what the
// relay does.
func (i Install) Automatic(p Preferences, getenv func(string) string) string {
	switch {
	case !i.updatable():
		return semconv.TermaUpdateAutoUnsupported
	case !p.Auto || OptedOut(getenv):
		return semconv.TermaUpdateAutoOff
	}
	return semconv.TermaUpdateAutoOn
}

// CanUpdate reports whether terma could ever install new releases here by itself, whatever
// the developer chose: in place of a release binary it replaces itself, or through the
// package manager of a Homebrew or global npm install, which it must be able to run
// (Manager.Argv). Not a source build, which has no release to follow, nor a project's npm
// dependency, whose lockfile is the project's, nor on Windows, where a running binary cannot
// be swapped yet and terma runs no package manager; nor a binary the process could not find.
// Nothing changes it while a process runs; whether it may write there now can (updatable).
func (i Install) CanUpdate() bool {
	if i.Exe == "" || i.goos == "windows" {
		return false
	}
	switch i.Method {
	case semconv.TermaInstallMethodScript:
		return true
	case semconv.TermaInstallMethodHomebrew, semconv.TermaInstallMethodNpm:
		return len(i.Manager.Argv) > 0
	}
	return false
}

// updatable reports whether terma can install new releases here by itself now: it could
// (CanUpdate), and this process may write where an update writes, which a root-owned npm
// prefix, another user's Homebrew, or a binary installed with sudo refuses. Asked at every
// heartbeat and pass, so permissions put right take effect at the next.
func (i Install) updatable() bool {
	return i.CanUpdate() && !slices.ContainsFunc(i.writes, procinfo.WriteDenied)
}

// Terma is where this install's terma is found after an update: the link its package
// manager moves to each version (Manager.Terma), never the versioned path an upgrade may
// remove, or the binary terma replaces in place.
func (i Install) Terma() string {
	if i.Manager.Terma != "" {
		return i.Manager.Terma
	}
	return i.Exe
}

// OptedOut reports whether the environment getenv reads keeps terma's own updates out of
// automation: CI set, or TERMA_NO_UPDATE_CHECK=1.
func OptedOut(getenv func(string) string) bool {
	return getenv("CI") != "" || getenv("TERMA_NO_UPDATE_CHECK") == "1"
}
