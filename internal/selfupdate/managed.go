package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Manager is the package manager that owns an installation. Those installations are
// upgraded through it, never replaced behind its back.
type Manager struct {
	// Name is how the developer knows it: "Homebrew" or "npm".
	Name string
	// Command is the upgrade as the developer would type it, for messages.
	Command string
	// Argv runs the upgrade with the package manager that owns this installation,
	// not whichever one PATH finds first. Empty when that program cannot be found or
	// the layout is not one terma recognizes; the developer is told Command instead.
	Argv []string
	// Terma is where the upgraded binary is found afterwards.
	Terma string
}

// ManagedBy reports the package manager that owns the binary at exe, if any. It reads
// only the path, so it costs nothing and runs nothing.
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
		// A new version lands in a new Caskroom or Cellar directory; the prefix's
		// bin/terma is the link Homebrew moves to it.
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
		// A global package lives in <prefix>/lib/node_modules on Unix; anything else is a
		// project's own dependency, which only that project's npm should change. The
		// prefix is passed explicitly, so an npm found on PATH (a custom prefix keeps no
		// npm of its own) still upgrades this copy and not another.
		if prefix, ok := strings.CutSuffix(before, "/lib"); ok && runtime.GOOS != "windows" {
			npm := filepath.FromSlash(prefix + "/bin/npm")
			if !isExecutable(npm) {
				npm, _ = exec.LookPath("npm")
			}
			if npm != "" {
				m.Argv = []string{npm, "install", "--global", "--prefix", filepath.FromSlash(prefix), "@miradorlabs/terma@latest"}
			}
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
