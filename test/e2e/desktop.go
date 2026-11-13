package e2e

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The desktop apps: Claude Desktop (Claude.app), whose Code tab runs Claude Code through the
// Agent SDK, and the Codex app (ChatGPT.app, com.openai.codex), which runs its threads in the
// `codex app-server` it bundles. Each is versioned by its own release, fetched from its
// vendor's update feed as the app's own updater fetches it, and its profile is taken by
// running the app itself (profile_desktop_test.go): the agent it starts, with the options and
// environment it gives it, is the app's own, and so is every request it makes. A profile can
// say only what the app did on the machine that ran it, so they run on a hosted CI runner (a
// fresh machine with a desktop session), never on a developer's: TERMA_E2E_DESKTOP_APPS=1
// opts in, and the scenarios change machine-wide state (managed preferences, a trusted CA)
// that they put back after.

// DesktopApp is a desktop app release, installed for a scenario.
type DesktopApp struct {
	Harness  string // "claude-desktop", "codex-desktop"
	Version  string // CFBundleShortVersionString, the release's own
	Build    string // CFBundleVersion
	BundleID string
	Name     string // the bundle's folder name, "Claude.app"
	Bundle   string // the .app
	URL      string
	Released time.Time
	// Signing is what the bundle's signature said before it first ran here.
	Signing *Signing
}

// Executable is the app's main executable.
func (a DesktopApp) Executable() string {
	exe, _ := plistValue(filepath.Join(a.Bundle, "Contents", "Info.plist"), "CFBundleExecutable")
	return filepath.Join(a.Bundle, "Contents", "MacOS", exe)
}

// desktopEnabled says whether this machine may run the desktop apps' profiles.
func desktopEnabled() (bool, string) {
	switch {
	case !Enabled():
		return false, "live tests run only with TERMA_E2E=1"
	case runtime.GOOS != "darwin" || runtime.GOARCH != "arm64":
		return false, "the desktop apps' profiles run on macOS on Apple silicon"
	case os.Getenv("TERMA_E2E_DESKTOP_APPS") != "1":
		return false, "the desktop apps' profiles change machine-wide settings: they run only with TERMA_E2E_DESKTOP_APPS=1, on a CI runner"
	}
	check, err := rootCommand("true")
	if err != nil {
		return false, "the desktop apps' profiles change machine-wide settings as root: " + err.Error()
	}
	if err := check.Run(); err != nil {
		return false, "the desktop apps' profiles need sudo without a password (managed preferences, the proxy's CA)"
	}
	return true, ""
}

// DesktopApps are the releases of harness's app to profile, installed: the versions
// TERMA_E2E_<HARNESS>_APP_VERSIONS lists ("latest", "lastN" or versions, as the feed has
// them), by default the newest.
func DesktopApps(t *testing.T, harness string) []DesktopApp {
	t.Helper()
	list := map[string]func() ([]DesktopRelease, error){"claude-desktop": claudeDesktopReleases, "codex-desktop": codexDesktopReleases}[harness]
	releases, err := list()
	if err != nil {
		t.Fatalf("read %s's releases: %v", harness, err)
	}
	if len(releases) == 0 {
		t.Fatalf("%s's update feed lists no releases", harness)
	}
	env := "TERMA_E2E_" + strings.ToUpper(strings.ReplaceAll(harness, "-", "_")) + "_APP_VERSIONS"
	spec := os.Getenv(env)
	if spec == "" {
		spec = "latest"
	}
	var picked []DesktopRelease
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
		case part == "latest":
			picked = append(picked, releases[len(releases)-1])
		case strings.HasPrefix(part, "last"):
			n, err := strconv.Atoi(strings.TrimPrefix(part, "last"))
			if err != nil || n <= 0 {
				t.Fatalf("%s: bad version spec %q", env, part)
			}
			picked = append(picked, releases[max(0, len(releases)-n):]...)
		default:
			i := slices.IndexFunc(releases, func(r DesktopRelease) bool { return r.Version == part })
			if i < 0 {
				t.Fatalf("%s %s is not in its update feed (%s)", harness, part, env)
			}
			picked = append(picked, releases[i])
		}
	}
	var out []DesktopApp
	for _, r := range sortReleases(picked) {
		app, err := ensureDesktopApp(harness, r)
		if err != nil {
			t.Fatalf("install %s %s: %v", harness, r.Version, err)
		}
		out = append(out, app)
	}
	return out
}

// forEachDesktopApp runs run for each release of harness's app to profile.
func forEachDesktopApp(t *testing.T, harness string, run func(t *testing.T, app DesktopApp)) {
	t.Helper()
	if ok, why := desktopEnabled(); !ok {
		t.Skip(why)
	}
	for _, app := range DesktopApps(t, harness) {
		t.Run("v"+app.Version, func(t *testing.T) { scenarioOfBuild(t, Binary{Harness: harness, Version: app.Version}); run(t, app) })
	}
}

// ensureDesktopApp downloads release's archive and unpacks its app, as the app's updater
// does (ditto keeps the bundle's signature), under versions/<harness>/<version>/, and reads
// its signature before it first runs.
func ensureDesktopApp(harness string, r DesktopRelease) (DesktopApp, error) {
	dir, err := filepath.Abs(filepath.Join(versionsDir(), harness, r.Version))
	if err != nil {
		return DesktopApp{}, err
	}
	app := DesktopApp{Harness: harness, URL: r.URL, Released: r.Released}
	if bundle := appIn(dir); bundle == "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return app, err
		}
		archive := filepath.Join(dir, "download.zip")
		if err := downloadFile(r.URL, archive); err != nil {
			return app, err
		}
		if out, err := exec.Command("ditto", "-x", "-k", archive, dir).CombinedOutput(); err != nil {
			return app, fmt.Errorf("unpack %s: %v: %s", r.URL, err, out)
		}
		_ = os.Remove(archive)
		bundle = appIn(dir)
		if bundle == "" {
			return app, fmt.Errorf("%s holds no app", r.URL)
		}
		s, err := inspectSigning(bundle)
		if err != nil {
			return app, err
		}
		if s != nil {
			data, _ := json.MarshalIndent(s, "", "  ")
			_ = os.WriteFile(signingSidecar(bundle), append(data, '\n'), 0o644)
		}
	}
	app.Bundle = appIn(dir)
	app.Name = filepath.Base(app.Bundle)
	info := filepath.Join(app.Bundle, "Contents", "Info.plist")
	app.Version, _ = plistValue(info, "CFBundleShortVersionString")
	app.Build, _ = plistValue(info, "CFBundleVersion")
	app.BundleID, _ = plistValue(info, "CFBundleIdentifier")
	if app.Version != r.Version {
		return app, fmt.Errorf("the feed's %s unpacked as version %s", r.Version, app.Version)
	}
	if data, err := os.ReadFile(signingSidecar(app.Bundle)); err == nil {
		var s Signing
		if json.Unmarshal(data, &s) == nil {
			app.Signing = &s
		}
	}
	return app, nil
}

// downloadFile writes what url serves to path, however large (an app's archive is most of a
// gigabyte, past what fetch keeps in memory), tried again as fetch tries.
func downloadFile(url, path string) error {
	client := &http.Client{Timeout: 30 * time.Minute}
	var err error
	for attempt := range fetchAttempts {
		if attempt > 0 {
			time.Sleep(fetchBackoff << (attempt - 1))
		}
		var resp *http.Response
		if resp, err = client.Get(url); err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			err = fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return err
			}
			continue
		}
		var f *os.File
		if f, err = os.Create(path); err != nil {
			resp.Body.Close()
			return err
		}
		_, err = io.Copy(f, resp.Body)
		resp.Body.Close()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && resp.ContentLength > 0 {
			if st, serr := os.Stat(path); serr == nil && st.Size() != resp.ContentLength {
				err = fmt.Errorf("%s: %d of %d bytes", url, st.Size(), resp.ContentLength)
			}
		}
		if err == nil {
			return nil
		}
	}
	return err
}

// appIn is the .app in dir, "" where there is none.
func appIn(dir string) string {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// plistValue is one top-level string of a property list.
func plistValue(path, key string) (string, error) {
	out, err := exec.Command("plutil", "-extract", key, "raw", "-o", "-", path).Output()
	return strings.TrimSpace(string(out)), err
}

// isAppBundle reports whether path is an application bundle.
func isAppBundle(path string) bool {
	st, err := os.Stat(filepath.Join(path, "Contents", "Info.plist"))
	return err == nil && !st.IsDir()
}

// --- What an app bundles --------------------------------------------------------------

// ClaudeDesktopPin is the Claude Code build a Claude Desktop release runs in its Code tab,
// as the release pins it: the version, the manifest's checksums, where it downloads it from,
// and the Agent SDK it drives it with.
type ClaudeDesktopPin struct {
	Version  string `json:"version"`
	Manifest struct {
		Commit    string `json:"commit"`
		BuildDate string `json:"buildDate"`
		Platforms map[string]struct {
			Binary   string `json:"binary"`
			Checksum string `json:"checksum"`
			Size     int64  `json:"size"`
			Bundle   *struct {
				Checksum string `json:"checksum"`
				Size     int64  `json:"size"`
			} `json:"bundle"`
		} `json:"platforms"`
	} `json:"manifest"`
	BaseURL           string `json:"baseUrl"`
	SDKWrapperVersion string `json:"sdkWrapperVersion"`
}

// claudePinJSON is the pin as the app's bundle declares it: JSON in a JavaScript string, its
// base URL Claude Code's releases or (from 4.23118.1) a release candidate's under them,
// `claude-code-releases/rc/<commit>`; never the SSH helper's pin beside it.
var claudePinJSON = regexp.MustCompile(`JSON\.parse\('(\{"version":"[^"']+","manifest":\{[^']*?"baseUrl":"[^"']*claude-code-releases(?:/[^"']*)?"[^']*?\})'\)`)

// claudeDesktopPinOf reads the pin from app's app.asar.
func claudeDesktopPinOf(app DesktopApp) (ClaudeDesktopPin, error) {
	var pin ClaudeDesktopPin
	files, err := readAsar(filepath.Join(app.Bundle, "Contents", "Resources", "app.asar"), func(name string) bool {
		return strings.HasPrefix(name, ".vite/build/") && strings.HasSuffix(name, ".js")
	})
	if err != nil {
		return pin, err
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range files {
			if !yield(k) {
				return
			}
		}
	}) {
		if m := claudePinJSON.FindSubmatch(files[name]); m != nil {
			if err := json.Unmarshal(m[1], &pin); err != nil {
				return pin, fmt.Errorf("%s: the Claude Code pin: %w", name, err)
			}
			return pin, nil
		}
	}
	return pin, errors.New("the app's bundle pins no Claude Code build")
}

// readAsar reads the files of an Electron archive that want names, by path, its unpacked ones
// aside.
func readAsar(path string, want func(string) bool) (map[string][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 16 {
		return nil, errors.New("asar: too short")
	}
	size := binary.LittleEndian.Uint32(data[4:8])
	if int(8+size) > len(data) {
		return nil, errors.New("asar: bad header")
	}
	header := data[8 : 8+size]
	n := binary.LittleEndian.Uint32(header[4:8])
	if int(8+n) > len(header) {
		return nil, errors.New("asar: bad header string")
	}
	type node struct {
		Files    map[string]*node `json:"files"`
		Offset   string           `json:"offset"`
		Size     int64            `json:"size"`
		Unpacked bool             `json:"unpacked"`
	}
	var root node
	if err := json.Unmarshal(header[8:8+n], &root); err != nil {
		return nil, fmt.Errorf("asar header: %w", err)
	}
	base := int64(8 + size)
	out := map[string][]byte{}
	var walk func(nd *node, prefix string)
	walk = func(nd *node, prefix string) {
		for name, c := range nd.Files {
			p := prefix + name
			if c.Files != nil {
				walk(c, p+"/")
				continue
			}
			if c.Unpacked || !want(p) {
				continue
			}
			off, err := strconv.ParseInt(c.Offset, 10, 64)
			if err != nil || base+off+c.Size > int64(len(data)) {
				continue
			}
			out[p] = data[base+off : base+off+c.Size]
		}
	}
	walk(&root, "")
	return out, nil
}

// CodexPackage is what the Codex app says of the Codex it bundles (codex-cli/codex-package.json).
type CodexPackage struct {
	Version    string `json:"version"`
	Target     string `json:"target"`
	Entrypoint string `json:"entrypoint"`
}

// codexDesktopCLI is the Codex app bundles, and the executable the app runs.
func codexDesktopCLI(app DesktopApp) (CodexPackage, string, error) {
	dir := filepath.Join(app.Bundle, "Contents", "Resources", "codex-cli")
	var pkg CodexPackage
	data, err := os.ReadFile(filepath.Join(dir, "codex-package.json"))
	if err != nil {
		return pkg, "", err
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return pkg, "", err
	}
	exe := filepath.Join(dir, "CodexCLI.app", "Contents", "MacOS", "codex")
	if _, err := os.Stat(exe); err != nil {
		exe = filepath.Join(dir, pkg.Entrypoint)
	}
	return pkg, exe, nil
}

// --- Running an app ----------------------------------------------------------------------

// RunningApp is a desktop app started for a scenario.
type RunningApp struct {
	App DesktopApp
	PID int
	t   *testing.T
	// Debug is where screenshots and the app's window are kept for a run to look at
	// afterwards ($TERMA_E2E_DESKTOP_DEBUG), "" for none.
	Debug string
	once  sync.Once
}

// LaunchDesktopApp starts app as the Finder does, through Launch Services (`open`), with the
// environment a login session gives the apps it starts (loginEnv) and none of the test's or the
// CI runner's, env added, and args on its command line, and waits for its process. It quits
// with t.
func LaunchDesktopApp(t *testing.T, app DesktopApp, env []string, args ...string) *RunningApp {
	t.Helper()
	cmd := []string{"-n", "-a", app.Bundle}
	for _, kv := range env {
		cmd = append(cmd, "--env", kv)
	}
	if len(args) > 0 {
		cmd = append(append(cmd, "--args"), args...)
	}
	// open hands the app its own environment: a login session's, not this process's.
	launch := desktopCmd("open", cmd...)
	launch.Env = loginEnv()
	if out, err := launch.CombinedOutput(); err != nil {
		t.Fatalf("open %s: %v: %s", app.Name, err, out)
	}
	r := &RunningApp{App: app, t: t, Debug: os.Getenv("TERMA_E2E_DESKTOP_DEBUG")}
	exe := app.Executable()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && r.PID == 0 {
		for _, p := range processes() {
			if p.Command == exe || strings.HasPrefix(p.Command, exe+" ") {
				r.PID = p.PID
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if r.PID == 0 {
		t.Fatalf("%s never started", app.Name)
	}
	if r.Debug != "" {
		r.Debug = filepath.Join(r.Debug, app.Harness+"-"+app.Version)
		_ = os.MkdirAll(r.Debug, 0o755)
	}
	t.Cleanup(r.Quit)
	return r
}

// Quit asks the app to quit, as its menu does, and ends it if it does not.
func (r *RunningApp) Quit() {
	r.once.Do(func() {
		_ = desktopCmd("osascript", "-e", fmt.Sprintf("tell application id %q to quit", r.App.BundleID)).Run()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) && unix.Kill(r.PID, 0) == nil {
			time.Sleep(500 * time.Millisecond)
		}
		_ = unix.Kill(r.PID, unix.SIGKILL)
		for _, p := range descendants(r.PID) {
			_ = unix.Kill(p.PID, unix.SIGKILL)
		}
	})
}

// Snap keeps a screenshot of the screen and the app's window (its accessibility tree) as
// name, where the run keeps them.
func (r *RunningApp) Snap(name string) {
	if r.Debug == "" {
		return
	}
	_ = desktopCmd("screencapture", "-x", filepath.Join(r.Debug, name+".png")).Run()
	if out, err := desktopUI(r.t, "dump", r.PID); err == nil {
		_ = os.WriteFile(filepath.Join(r.Debug, name+".ax.txt"), []byte(out), 0o644)
	}
}

// Keep copies what the run keeps of path (a log the app wrote) into the debug folder.
func (r *RunningApp) Keep(path string) {
	if r.Debug == "" {
		return
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".log") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err == nil {
			_ = os.WriteFile(filepath.Join(r.Debug, filepath.Base(p)), data, 0o644)
		}
		return nil
	})
}

var (
	uiOnce sync.Once
	uiPath string
	uiErr  error
)

// desktopUI runs the accessibility driver (desktop/ui.swift), built once a run.
func desktopUI(t *testing.T, command string, pid int, args ...string) (string, error) {
	uiOnce.Do(func() {
		dir, err := os.MkdirTemp("", "terma-desktopui")
		if err != nil {
			uiErr = err
			return
		}
		src, err := filepath.Abs(filepath.Join("desktop", "ui.swift"))
		if err != nil {
			uiErr = err
			return
		}
		uiPath = filepath.Join(dir, "desktopui")
		if out, err := exec.Command("xcrun", "swiftc", "-O", src, "-o", uiPath).CombinedOutput(); err != nil {
			uiErr = fmt.Errorf("build the accessibility driver: %v: %s", err, out)
		}
	})
	if uiErr != nil {
		return "", uiErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, uiPath, append([]string{command, strconv.Itoa(pid)}, args...)...).CombinedOutput()
	return string(out), err
}

// Press presses the first element of the app's window labelled label, waiting up to timeout
// for it to be there.
func (r *RunningApp) Press(label string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		out, err := desktopUI(r.t, "press", r.PID, label)
		if err == nil {
			return true
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			r.t.Fatalf("press %q in %s: %v: %s", label, r.App.Name, err, out)
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

// Key presses a key (return, tab, escape) with the app in front.
func (r *RunningApp) Key(name string) {
	if out, err := desktopUI(r.t, "key", r.PID, name); err != nil {
		r.t.Fatalf("press %s in %s: %v: %s", name, r.App.Name, err, out)
	}
}

// Window is the app's window as the accessibility API reads it, one element a line.
func (r *RunningApp) Window() string {
	out, _ := desktopUI(r.t, "dump", r.PID)
	return out
}
