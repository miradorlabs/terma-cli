package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Manager is the package manager that owns an installation, which is upgraded through it.
type Manager struct {
	// Name is "Homebrew" or "npm".
	Name string
	// Command is the upgrade as the developer would type it.
	Command string
	// Argv runs the owning manager itself, not PATH's; empty when it cannot be found.
	Argv []string
	// Terma is where the upgraded binary is found afterwards.
	Terma string
	// Project is set for a project dependency: Command runs there, never by terma.
	Project string
}

// ManagedBy reports the package manager that owns the binary at exe, from its path alone.
func ManagedBy(exe string) (Manager, bool) {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	path := filepath.ToSlash(exe)
	for _, kind := range []struct{ dir, cask string }{{"/Caskroom/terma/", "--cask"}, {"/Cellar/terma/", ""}} {
		prefix, _, ok := strings.Cut(path, kind.dir)
		if !ok {
			continue
		}
		// The prefix's bin/terma is the link that moves to each new version.
		m := Manager{Name: "Homebrew", Command: "brew upgrade terma", Terma: filepath.FromSlash(prefix + "/bin/terma")}
		if brew := filepath.FromSlash(prefix + "/bin/brew"); isExecutable(brew) {
			m.Argv = []string{brew, "upgrade"}
			if kind.cask != "" {
				m.Argv = append(m.Argv, kind.cask)
			}
			m.Argv = append(m.Argv, "terma")
		}
		return m, true
	}
	if before, _, ok := strings.Cut(path, "/node_modules/@miradorlabs/terma/"); ok {
		m := Manager{Name: "npm", Command: "npm install -g @miradorlabs/terma@latest", Terma: exe}
		// On Windows terma neither runs npm nor tells the layouts apart.
		if runtime.GOOS == "windows" {
			return m, true
		}
		// Outside <prefix>/lib/node_modules it is a project's dependency, and its lockfile is the project's.
		prefix, global := strings.CutSuffix(before, "/lib")
		if !global {
			m.Command, m.Project = "npm install @miradorlabs/terma@latest", filepath.FromSlash(before)
			return m, true
		}
		// An explicit prefix makes a PATH npm upgrade this copy, not another.
		npm := filepath.FromSlash(prefix + "/bin/npm")
		if !isExecutable(npm) {
			npm, _ = exec.LookPath("npm")
		}
		if npm != "" {
			m.Argv = []string{npm, "install", "--global", "--prefix", filepath.FromSlash(prefix), "@miradorlabs/terma@latest"}
		}
		return m, true
	}
	return Manager{}, false
}

// ManagedCommand returns the owning package manager's upgrade command, if any.
func ManagedCommand(exe string) string {
	m, _ := ManagedBy(exe)
	return m.Command
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}
