package doctor

import (
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/output"
	"github.com/miradorlabs/terma-cli/internal/shellrc"
)

// WellKnownBinDirs are where a terma binary gets installed besides wherever PATH points
// today: the install script's and Homebrew's directories, Go's, and the system one that
// apps started outside a shell search first.
func WellKnownBinDirs() []string {
	dirs := []string{"/usr/local/bin", "/opt/homebrew/bin", "/usr/bin"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"), filepath.Join(home, "go", "bin"))
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		dirs = append(dirs, filepath.Join(filepath.SplitList(gopath)[0], "bin"))
	}
	return dirs
}

// otherTermas lists the terma binaries on this machine that are a different build from
// primary (the one `terma` resolves to here), each with when it was installed. It looks
// along PATH and in binDirs, never in terma's own shim directory, and compares
// contents — it does not run what it finds. A copy of the same build, or a link to the
// same file, is not reported: having two is only a problem when they disagree.
func otherTermas(primary string, binDirs []string) []string {
	want, err := installedBinaryDigest(primary)
	if err != nil {
		return nil
	}
	primaryInfo, _ := os.Stat(primary)
	name := "terma"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var out []string
	seen := map[string]bool{}
	for _, dir := range append(filepath.SplitList(os.Getenv("PATH")), binDirs...) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate) // follows symlinks: a link to primary is primary
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 || os.SameFile(info, primaryInfo) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil || seen[resolved] {
			continue
		}
		seen[resolved] = true
		if got, err := installedBinaryDigest(candidate); err != nil || got == want {
			continue
		}
		out = append(out, output.TildePath(candidate)+" (installed "+info.ModTime().Format("2006-01-02 15:04")+")")
	}
	return out
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// npmLauncherDigest pins the published npm launcher. A path named terma in an
// npm layout is not enough to trust it: the launcher is executable JavaScript,
// and doctor must never run a PATH candidate to learn where it points.
const npmLauncherDigest = "a90d18d5df9946c37f39c80fe34f178202a1f65bf58abffaf0eb1f6f4f15cedb"

// InstalledBinary resolves only the official npm launcher to its vendor binary.
// For every other PATH entry, including an edited npm launcher, compare the file
// itself. npm's bin link and the package-local bin file both lead to this path.
func InstalledBinary(path string) string {
	if runtime.GOOS == "windows" {
		return path
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !strings.HasSuffix(filepath.ToSlash(resolved), "/node_modules/@miradorlabs/terma/bin/terma.js") {
		return path
	}
	if digest, err := fileDigest(resolved); err != nil || digest != npmLauncherDigest {
		return path
	}
	vendor := filepath.Clean(filepath.Join(filepath.Dir(resolved), "..", "vendor", "terma"))
	info, err := os.Stat(vendor)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return path
	}
	return vendor
}

func installedBinaryDigest(path string) (string, error) {
	return fileDigest(InstalledBinary(path))
}

// AddToPathCommand is the command a developer runs to put dir on PATH for good, in their
// own shell: the line appended to the startup file terma knows for it and read into this
// shell, or for fish, fish_add_path, which keeps the entry itself. A shell terma does not
// know gets the line for this shell alone. terma never runs it: this is the developer's
// own startup file.
func AddToPathCommand(dir string) string {
	rc, ok := shellrc.ShellRC()
	if !ok {
		return `export PATH="` + dir + `:$PATH"`
	}
	line := rc.PathLine(dir)
	if rc.Shell == "fish" {
		return line
	}
	file := shellPath(rc.Path)
	return "echo '" + strings.ReplaceAll(line, "'", `'\''`) + "' >> " + file + " && " + ReloadCommand(file)
}

// shellPath writes a path for a command line: ~/… when that needs no quoting, else the
// full path in single quotes.
func shellPath(path string) string {
	const plain = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-~"
	if short := output.TildePath(path); !strings.ContainsFunc(short, func(r rune) bool { return !strings.ContainsRune(plain, r) }) {
		return short
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// BinaryCheck checks that hooks find terma by name and run exe's build, looking for
// other builds along PATH and in binDirs.
func BinaryCheck(exe string, binDirs []string) Check {
	path, err := exec.LookPath("terma")
	if err != nil {
		return Check{Status: Fail, Detail: "hooks call `terma` by name and will not find it",
			Fix: "run `" + AddToPathCommand(filepath.Dir(exe)) + "` to put " + filepath.Dir(exe) + " on PATH (or reinstall with the install script)"}
	}
	if current, err := fileDigest(exe); err == nil {
		if installed, err := installedBinaryDigest(path); err == nil && current != installed {
			return Check{Status: Warn,
				Detail: path + binaryBuildLabel(path) + "; hooks run a different build from " + exe,
				Fix:    "run `" + AddToPathCommand(filepath.Dir(exe)) + "` to put " + filepath.Dir(exe) + " first on PATH, or replace " + path + " with this build; then run `terma doctor`"}
		}
	}
	// Hooks call `terma` by name, and the name does not resolve the same way
	// everywhere: an app started from the Dock gets the system's PATH, not the
	// shell's, so a second copy in /usr/local/bin is the one Cursor's hooks run. When
	// that copy is another build, the same repository behaves two ways depending on
	// where the agent was launched — and a build from before .terma/settings.json
	// does not see the binding at all.
	if others := otherTermas(path, binDirs); len(others) > 0 {
		return Check{Status: Warn,
			Detail: path + binaryBuildLabel(path) + "; a different build is also installed: " + strings.Join(others, ", "),
			Fix:    "replace or remove the other copy — an agent started outside this shell (from the Dock, an IDE) can resolve `terma` to it"}
	}
	return Check{Status: Pass, Detail: path + binaryBuildLabel(path)}
}

// binaryBuildLabel reads build metadata without running an executable found on PATH.
func binaryBuildLabel(path string) string {
	info, err := buildinfo.ReadFile(InstalledBinary(path))
	if err != nil {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			return " (build " + setting.Value[:min(7, len(setting.Value))] + ")"
		}
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return " (" + info.Main.Version + ")"
	}
	return ""
}

// ReloadCommand re-reads a startup file in the running shell: `source` where the shell
// has it (zsh, bash, fish), the POSIX `.` otherwise.
func ReloadCommand(file string) string {
	switch filepath.Base(os.Getenv("SHELL")) {
	case "zsh", "bash", "fish":
		return "source " + file
	}
	return ". " + file
}
