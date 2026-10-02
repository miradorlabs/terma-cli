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

	termaproject "github.com/miradorlabs/terma-cli/internal/project"
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

// HookCaller is how the hooks in play reach terma.
type HookCaller int

const (
	// ByFullPath is machine-wide hooks only: setup writes terma's absolute path into them.
	ByFullPath HookCaller = iota
	// ByName is a repository's committed hooks, which run `terma` from PATH.
	ByName
)

// agentHookDirs are the directories a committed agent hook adds to the system PATH an agent
// started from the Dock or an IDE gets; a var so tests can stand in.
var agentHookDirs = func() []string {
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append([]string{filepath.Join(home, ".local", "bin")}, dirs...)
	}
	return dirs
}

// HookCallerFor is how hooks reach terma in the workspace at root: a bound repository's
// committed hooks call it by name, and elsewhere only machine-wide hooks run.
func HookCallerFor(root, gitDir string, repoErr error) HookCaller {
	if repoErr == nil {
		if _, _, err := termaproject.Resolve(root, gitDir); err == nil {
			return ByName
		}
	}
	return ByFullPath
}

// BinaryCheck checks that the hooks in play run exe's build, looking for other builds along
// PATH and in binDirs. Only committed hooks look terma up by name.
func BinaryCheck(exe string, binDirs []string, caller HookCaller) Check {
	path, err := exec.LookPath("terma")
	if err != nil {
		if caller == ByFullPath {
			return Check{Status: Pass, Detail: "not on PATH; machine-wide hooks run terma by the full path setup wrote into them (a repository's committed hooks need it on PATH)"}
		}
		return Check{Status: Fail, Detail: "this repository's hooks call `terma` by name and will not find it",
			Fix: "run `" + AddToPathCommand(filepath.Dir(exe)) + "` to put " + filepath.Dir(exe) + " on PATH (or reinstall with the install script)"}
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
	if caller == ByName && runtime.GOOS != "windows" && !onAgentHookPath() {
		return Check{Status: Warn,
			Detail: path + binaryBuildLabel(path) + "; an agent started from the Dock or an IDE looks only in " + tildeList(agentHookDirs()) + ", so its hooks will not find it",
			Fix:    "run `mkdir -p ~/.local/bin && ln -sf " + shellPath(path) + " ~/.local/bin/terma`"}
	}
	return Check{Status: Pass, Detail: path + binaryBuildLabel(path)}
}

func tildeList(paths []string) string {
	short := make([]string, len(paths))
	for i, p := range paths {
		short[i] = output.TildePath(p)
	}
	return strings.Join(short, ", ")
}

// onAgentHookPath reports whether a committed hook run with an agent's system PATH finds terma.
func onAgentHookPath() bool {
	for _, dir := range agentHookDirs() {
		if info, err := os.Stat(filepath.Join(dir, "terma")); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
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
