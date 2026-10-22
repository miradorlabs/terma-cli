package selfupdate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// The install method is told from the binary's path, through a link to it, and the version,
// which comes first: a build that is not a release is a source build wherever it sits.
func TestInstallOfTellsHowTermaWasInstalled(t *testing.T) {
	unixLayouts(t)
	t.Setenv("PATH", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brew := filepath.Join(root, "brew")
	executable(t, filepath.Join(brew, "bin", "brew"))
	cask := filepath.Join(brew, "Caskroom", "terma", "1.0.0", "terma")
	executable(t, cask)
	// Homebrew's bin/terma is a link to the version it installed: the relay may start from it.
	if err := os.Symlink(cask, filepath.Join(brew, "bin", "terma")); err != nil {
		t.Fatal(err)
	}
	npm := filepath.Join(root, "node")
	executable(t, filepath.Join(npm, "bin", "npm"))

	for _, tc := range []struct {
		name, exe, version, goos, want string
	}{
		{"cask", cask, "1.0.0", "darwin", semconv.TermaInstallMethodHomebrew},
		{"cask, through Homebrew's link", filepath.Join(brew, "bin", "terma"), "1.0.0", "darwin", semconv.TermaInstallMethodHomebrew},
		{"formula", filepath.Join(brew, "Cellar", "terma", "1.0.0", "bin", "terma"), "1.0.0", "linux", semconv.TermaInstallMethodHomebrew},
		{"global npm", filepath.Join(npm, "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "1.0.0", "linux", semconv.TermaInstallMethodNpm},
		{"project npm", filepath.Join(root, "app", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "1.0.0", "darwin", semconv.TermaInstallMethodNpmProject},
		{"install script", filepath.Join(root, "home", ".local", "bin", "terma"), "1.0.0", "darwin", semconv.TermaInstallMethodScript},
		{"TERMA_INSTALL_DIR or by hand", filepath.Join(root, "opt", "tools", "terma"), "1.0.0-rc.1", "linux", semconv.TermaInstallMethodScript},
		{"source build", filepath.Join(root, "go", "bin", "terma"), "v1.0.0-3-g401af35", "darwin", semconv.TermaInstallMethodSource},
		{"source build in a package manager's place", cask, "dev", "darwin", semconv.TermaInstallMethodSource},
		{"source build in npm's place", filepath.Join(npm, "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "1.0.0-dirty", "linux", semconv.TermaInstallMethodSource},
		// Windows: any npm layout is npm, which terma does not tell apart there.
		{"windows npm", "C:/Users/d/AppData/Roaming/npm/node_modules/@miradorlabs/terma/vendor/terma.exe", "1.0.0", "windows", semconv.TermaInstallMethodNpm},
		{"windows npm project", "C:/src/app/node_modules/@miradorlabs/terma/vendor/terma.exe", "1.0.0", "windows", semconv.TermaInstallMethodNpm},
		{"windows archive", "C:/Users/d/bin/terma.exe", "1.0.0", "windows", semconv.TermaInstallMethodScript},
		{"not found", "", "1.0.0", "darwin", ""},
		{"source build not found", "", "dev", "darwin", semconv.TermaInstallMethodSource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InstallOn(tc.exe, tc.version, tc.goos).Method; got != tc.want {
				t.Fatalf("InstallOn(%s, %s, %s).Method = %q, want %q", tc.exe, tc.version, tc.goos, got, tc.want)
			}
		})
	}
	if i := InstallOf(cask, "1.0.0"); i.Manager.Name != "Homebrew" || i.Manager.Terma != filepath.Join(brew, "bin", "terma") {
		t.Fatalf("a Homebrew install's manager = %+v", i.Manager)
	}
}

// terma.update.auto is on for a release binary terma replaces itself, or a Homebrew or global
// npm install whose package manager it can run, with automatic updates on; off where only the
// developer's choice or the environment stops it; and unsupported where nothing the
// developer chooses would make it update, whatever they chose.
func TestAutomaticIsTheRelaysOneRule(t *testing.T) {
	unixLayouts(t)
	t.Setenv("PATH", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable(t, filepath.Join(root, "brew", "bin", "brew"))
	executable(t, filepath.Join(root, "node", "bin", "npm"))
	script := InstallOn(filepath.Join(root, ".local", "bin", "terma"), "1.0.0", "darwin")
	homebrew := InstallOn(filepath.Join(root, "brew", "Caskroom", "terma", "1.0.0", "terma"), "1.0.0", "darwin")
	npm := InstallOn(filepath.Join(root, "node", "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "1.0.0", "linux")
	if len(homebrew.Manager.Argv) == 0 || len(npm.Manager.Argv) == 0 {
		t.Fatalf("the package managers were not found: %+v, %+v", homebrew.Manager, npm.Manager)
	}
	on, off := Preferences{Auto: true}, Preferences{Auto: false}
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	none := env(nil)
	for _, tc := range []struct {
		name    string
		install Install
		p       Preferences
		getenv  func(string) string
		want    string
	}{
		{"install script", script, on, none, semconv.TermaUpdateAutoOn},
		{"turned off", script, off, none, semconv.TermaUpdateAutoOff},
		{"CI", script, on, env(map[string]string{"CI": "true"}), semconv.TermaUpdateAutoOff},
		{"opted out of checks", script, on, env(map[string]string{"TERMA_NO_UPDATE_CHECK": "1"}), semconv.TermaUpdateAutoOff},
		{"checks not opted out", script, on, env(map[string]string{"TERMA_NO_UPDATE_CHECK": "0"}), semconv.TermaUpdateAutoOn},
		{"homebrew", homebrew, on, none, semconv.TermaUpdateAutoOn},
		{"homebrew, turned off", homebrew, off, none, semconv.TermaUpdateAutoOff},
		{"homebrew in CI", homebrew, on, env(map[string]string{"CI": "1"}), semconv.TermaUpdateAutoOff},
		{"global npm", npm, on, none, semconv.TermaUpdateAutoOn},
		{"global npm, turned off", npm, off, none, semconv.TermaUpdateAutoOff},
		// Its binary is replaced in place: npm itself is never needed.
		{"global npm without npm", InstallOn(filepath.Join(root, "custom", "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "1.0.0", "linux"), on, none, semconv.TermaUpdateAutoOn},
		// terma runs the package manager that owns the install, never another: without it,
		// the developer is told the command.
		{"homebrew not found", InstallOn(filepath.Join(root, "elsewhere", "Caskroom", "terma", "1.0.0", "terma"), "1.0.0", "darwin"), on, none, semconv.TermaUpdateAutoUnsupported},
		{"windows npm", InstallOn("C:/Users/d/AppData/Roaming/npm/node_modules/@miradorlabs/terma/vendor/terma.exe", "1.0.0", "windows"), on, none, semconv.TermaUpdateAutoUnsupported},
		{"npm project", InstallOn(filepath.Join(root, "app", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "1.0.0", "darwin"), on, none, semconv.TermaUpdateAutoUnsupported},
		{"source build", InstallOn(filepath.Join(root, ".local", "bin", "terma"), "dev", "darwin"), on, none, semconv.TermaUpdateAutoUnsupported},
		{"windows", InstallOn("C:/Users/d/bin/terma.exe", "1.0.0", "windows"), on, none, semconv.TermaUpdateAutoUnsupported},
		{"binary not found", InstallOn("", "1.0.0", "darwin"), on, none, semconv.TermaUpdateAutoUnsupported},
		// Never off for an install that could not update anyway: the platform counts what
		// would follow if the developer turned updates back on.
		{"source build, turned off", InstallOn(filepath.Join(root, ".local", "bin", "terma"), "dev", "darwin"), off, none, semconv.TermaUpdateAutoUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.install.Automatic(tc.p, tc.getenv); got != tc.want {
				t.Fatalf("Automatic = %q, want %q", got, tc.want)
			}
		})
	}
}

// An install terma may not write, a root-owned npm prefix (its package's vendor folder),
// another user's Homebrew or a binary installed with sudo, never updates by itself, however often it tried: unsupported,
// until its permissions are put right.
func TestAnInstallTermaMayNotWriteIsUnsupported(t *testing.T) {
	unixLayouts(t)
	if os.Geteuid() == 0 {
		t.Skip("root may write anywhere")
	}
	t.Setenv("PATH", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brew := filepath.Join(root, "brew")
	executable(t, filepath.Join(brew, "bin", "brew"))
	cask := filepath.Join(brew, "Caskroom", "terma", "1.0.0", "terma")
	executable(t, cask)
	npm := filepath.Join(root, "node")
	executable(t, filepath.Join(npm, "bin", "npm"))
	pkg := filepath.Join(npm, "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma")
	executable(t, pkg)
	script := filepath.Join(root, "usr", "local", "bin", "terma")
	executable(t, script)
	on := Preferences{Auto: true}
	none := func(string) string { return "" }
	for _, tc := range []struct{ name, exe, locked, goos string }{
		{"homebrew", cask, filepath.Join(brew, "Caskroom", "terma"), "darwin"},
		{"homebrew's bin", cask, filepath.Join(brew, "bin"), "darwin"},
		{"global npm", pkg, filepath.Dir(pkg), "linux"},
		{"install script", script, filepath.Dir(script), "linux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := InstallOn(tc.exe, "1.0.0", tc.goos)
			if got := i.Automatic(on, none); got != semconv.TermaUpdateAutoOn {
				t.Fatalf("writable: %q", got)
			}
			if err := os.Chmod(tc.locked, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(tc.locked, 0o755) })
			if got := i.Automatic(on, none); got != semconv.TermaUpdateAutoUnsupported || !i.CanUpdate() {
				t.Fatalf("with %s read-only: %q, CanUpdate %v", tc.locked, got, i.CanUpdate())
			}
			if err := os.Chmod(tc.locked, 0o755); err != nil {
				t.Fatal(err)
			}
			if got := i.Automatic(on, none); got != semconv.TermaUpdateAutoOn {
				t.Fatalf("put right: %q", got)
			}
		})
	}
}
