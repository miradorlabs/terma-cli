package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

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

// InstallOf tells how the terma at exe, at version, was installed, from its path, version
// and, for npm, its global install's link (ManagedBy). The version comes first: a build that is not a release (IsRelease) is a source
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
		i.Method, i.writes = semconv.TermaInstallMethodScript, inPlace(exe)
	case m.Name == "Homebrew":
		i.Method, i.Manager, i.writes = semconv.TermaInstallMethodHomebrew, m, m.writes
	case m.Project != "":
		i.Method, i.Manager = semconv.TermaInstallMethodNpmProject, m
	default:
		// The npm package is a shim that runs its vendor binary, which is the one terma
		// replaces in place, as the install script's.
		i.Method, i.Manager, i.writes = semconv.TermaInstallMethodNpm, m, inPlace(exe)
	}
	return i
}

// inPlace is where an update in place of the binary at exe writes: beside it, its link
// resolved.
func inPlace(exe string) []string {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return []string{filepath.Dir(exe)}
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
// the developer chose: in place of a release binary, the install script's or a global npm
// package's, whose shim only runs it, so the relay never needs npm or node; or through
// Homebrew, whose own brew it must find (Manager.Argv). Not a source build, which has no
// release to follow, nor a project's npm dependency, whose lockfile is the project's, nor
// on Windows, where a running binary cannot be swapped yet; nor a binary the process could
// not find. Nothing changes it while a process runs; whether it may write there now can
// (updatable).
func (i Install) CanUpdate() bool {
	if i.Exe == "" || i.goos == "windows" {
		return false
	}
	switch i.Method {
	case semconv.TermaInstallMethodScript, semconv.TermaInstallMethodNpm:
		return true
	case semconv.TermaInstallMethodHomebrew:
		return len(i.Manager.Argv) > 0
	}
	return false
}

// throughHomebrew reports whether new releases are installed through Homebrew rather than in
// place of the binary.
func (i Install) throughHomebrew() bool {
	return i.Method == semconv.TermaInstallMethodHomebrew
}

// through is the package manager that installs new releases here (Updated.Through):
// "Homebrew", or "" for an update in place.
func (i Install) through() string {
	if i.throughHomebrew() {
		return i.Manager.Name
	}
	return ""
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

// installedAt is when the release in place here was put there, as the file system has it:
// for Homebrew, the directory it installed the binary in, which each version gets anew
// (Caskroom/terma/<version>, or Cellar/terma/<version>/bin); else the binary terma wrote in
// place; zero when it cannot be told.
func (i Install) installedAt() time.Time {
	path := i.Terma()
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if i.throughHomebrew() {
		path = filepath.Dir(path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
