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

	"github.com/miradorlabs/terma-cli/internal/shellrc"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// WellKnownBinDirs are where a terma binary gets installed besides wherever PATH points.
func WellKnownBinDirs() []string {
	dirs := []string{"/usr/local/bin", "/opt/homebrew/bin", "/usr/bin", "/bin"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"), filepath.Join(home, "go", "bin"))
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		dirs = append(dirs, filepath.Join(filepath.SplitList(gopath)[0], "bin"))
	}
	return dirs
}

// otherTermas lists the terma binaries along PATH and in binDirs that are a different
// build from primary, comparing contents and never running what it finds.
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
		info, err := os.Stat(candidate)
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

// npmLauncherDigest pins the published npm launcher, which doctor must never run to
// learn where it points.
const npmLauncherDigest = "a90d18d5df9946c37f39c80fe34f178202a1f65bf58abffaf0eb1f6f4f15cedb"

// InstalledBinary resolves the official npm launcher to its vendor binary, and anything
// else, an edited launcher included, to itself.
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

// AddToPathCommand is the command a developer runs to put dir on PATH for good in their
// own shell; terma never runs it.
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

// shellPath writes ~/… when that needs no quoting, else the full path in single quotes.
func shellPath(path string) string {
	const plain = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-~"
	if short := output.TildePath(path); !strings.ContainsFunc(short, func(r rune) bool { return !strings.ContainsRune(plain, r) }) {
		return short
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// BinaryCheck checks the terma on PATH is exe's build, looking for other builds along PATH
// and in binDirs; the hooks themselves run terma by the full path setup wrote into them.
func BinaryCheck(exe string, binDirs []string) Check {
	path, err := exec.LookPath("terma")
	if err != nil {
		return Check{Status: Pass, Detail: "not on PATH; the machine-wide hooks run terma by the full path setup wrote into them"}
	}
	if current, err := fileDigest(exe); err == nil {
		if installed, err := installedBinaryDigest(path); err == nil && current != installed {
			return Check{Status: Warn,
				Detail: path + binaryBuildLabel(path) + "; hooks run a different build from " + exe,
				Fix:    "run `" + AddToPathCommand(filepath.Dir(exe)) + "` to put " + filepath.Dir(exe) + " first on PATH, or replace " + path + " with this build; then run `terma doctor`"}
		}
	}
	// An app started from the Dock gets the system PATH, so its hooks may run a second
	// copy in /usr/local/bin instead of this one.
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

// ReloadCommand re-reads a startup file in the running shell.
func ReloadCommand(file string) string {
	switch filepath.Base(os.Getenv("SHELL")) {
	case "zsh", "bash", "fish":
		return "source " + file
	}
	return ". " + file
}
