package selfupdate

import (
	"runtime"

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
}

// InstallOf tells how the terma at exe, at version, was installed, from its path and version
// alone. The version comes first: a build that is not a release (IsRelease) is a source
// build wherever it is, a package manager's directory included. An exe of "" is a binary
// the process could not find: a release's has no method, and is never updated (Automatic).
func InstallOf(exe, version string) Install {
	return installOf(exe, version, runtime.GOOS)
}

func installOf(exe, version, goos string) Install {
	i := Install{Exe: exe, goos: goos}
	m, managed := managedBy(exe, goos)
	switch {
	case !IsRelease(version):
		i.Method = semconv.TermaInstallMethodSource
	case exe == "":
	case !managed:
		i.Method = semconv.TermaInstallMethodScript
	case m.Name == "Homebrew":
		i.Method, i.Manager = semconv.TermaInstallMethodHomebrew, m
	case m.Project != "":
		i.Method, i.Manager = semconv.TermaInstallMethodNpmProject, m
	default:
		i.Method, i.Manager = semconv.TermaInstallMethodNpm, m
	}
	return i
}

// Automatic is terma.update.auto for this install, under the developer's choice p and the
// environment getenv reads: whether the relay installs each new release by itself
// (semconv.TermaUpdateAutoOn), could but is told not to (Off: `terma update --auto off`, or
// CI or TERMA_NO_UPDATE_CHECK=1, which keep terma's updates out of automation), or never
// can, whatever the developer chooses (Unsupported). It is the one rule: the relay's
// updater installs releases exactly when it says on, so what the heartbeat reports is what
// the relay does. Only a release binary terma replaces itself is supported: a source build
// has no release to follow, a package manager's install is that manager's to upgrade, and
// a running Windows binary cannot be swapped yet.
func (i Install) Automatic(p Preferences, getenv func(string) string) string {
	switch {
	case i.Exe == "" || i.Method != semconv.TermaInstallMethodScript || i.goos == "windows":
		return semconv.TermaUpdateAutoUnsupported
	case !p.Auto || getenv("CI") != "" || getenv("TERMA_NO_UPDATE_CHECK") == "1":
		return semconv.TermaUpdateAutoOff
	}
	return semconv.TermaUpdateAutoOn
}
