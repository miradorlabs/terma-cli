package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
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
	"sort"
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

// DesktopRelease is one release of a desktop app, as its update feed lists it.
type DesktopRelease struct {
	Version  string
	Build    string // the feed's build number, where it has one (Sparkle's)
	URL      string
	Released time.Time
}

const (
	claudeDesktopFeed = "https://downloads.claude.ai/releases/darwin/universal/RELEASES.json"
	codexDesktopFeed  = "https://persistent.oaistatic.com/codex-app-prod/appcast.xml"
)

// DesktopFeeds are the update feeds the desktop apps' updaters read, by harness.
var DesktopFeeds = map[string]string{"claude-desktop": claudeDesktopFeed, "codex-desktop": codexDesktopFeed}

// ParseDesktopFeed reads the releases harness's update feed lists, oldest first.
func ParseDesktopFeed(harness string, data []byte) ([]DesktopRelease, error) {
	if harness == "claude-desktop" {
		return parseSquirrelFeed(data)
	}
	return parseAppcast(data)
}

// claudeDesktopReleases are the releases Claude Desktop's updater offers (Squirrel's
// RELEASES.json): the current one alone.
func claudeDesktopReleases() ([]DesktopRelease, error) {
	data, err := fetch(claudeDesktopFeed)
	if err != nil {
		return nil, err
	}
	return parseSquirrelFeed(data)
}

// parseSquirrelFeed reads a Squirrel.Mac RELEASES.json.
func parseSquirrelFeed(data []byte) ([]DesktopRelease, error) {
	var feed struct {
		Releases []struct {
			UpdateTo struct {
				Version string    `json:"version"`
				URL     string    `json:"url"`
				PubDate time.Time `json:"pub_date"`
			} `json:"updateTo"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(data, &feed); err != nil {
		return nil, fmt.Errorf("RELEASES.json: %w", err)
	}
	var out []DesktopRelease
	for _, r := range feed.Releases {
		if u := r.UpdateTo; u.Version != "" && u.URL != "" {
			out = append(out, DesktopRelease{Version: u.Version, URL: u.URL, Released: u.PubDate})
		}
	}
	return sortReleases(out), nil
}

// codexDesktopReleases are the releases the Codex app's Sparkle feed lists, its full archives
// (not the deltas between them).
func codexDesktopReleases() ([]DesktopRelease, error) {
	data, err := fetch(codexDesktopFeed)
	if err != nil {
		return nil, err
	}
	return parseAppcast(data)
}

// parseAppcast reads a Sparkle appcast's items.
func parseAppcast(data []byte) ([]DesktopRelease, error) {
	var feed struct {
		Items []struct {
			PubDate   string `xml:"pubDate"`
			Version   string `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle version"`
			Short     string `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle shortVersionString"`
			Enclosure struct {
				URL string `xml:"url,attr"`
			} `xml:"enclosure"`
		} `xml:"channel>item"`
	}
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, err
	}
	var out []DesktopRelease
	for _, it := range feed.Items {
		if it.Short == "" || it.Enclosure.URL == "" {
			continue
		}
		at, _ := time.Parse(time.RFC1123Z, it.PubDate)
		out = append(out, DesktopRelease{Version: it.Short, Build: it.Version, URL: it.Enclosure.URL, Released: at.UTC()})
	}
	return sortReleases(out), nil
}

// sortReleases orders releases oldest first, one a version.
func sortReleases(rs []DesktopRelease) []DesktopRelease {
	sort.SliceStable(rs, func(i, j int) bool { return versionLess(rs[i].Version, rs[j].Version) })
	return slices.CompactFunc(rs, func(a, b DesktopRelease) bool { return a.Version == b.Version })
}

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
	if err := desktopCmd("sudo", "-n", "true").Run(); err != nil {
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
		t.Run("v"+app.Version, func(t *testing.T) { scenarioOf(t, harness); run(t, app) })
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

// claudePinJSON is the pin as the app's bundle declares it: JSON in a JavaScript string.
var claudePinJSON = regexp.MustCompile(`JSON\.parse\('(\{"version":"[^"']+","manifest":\{[^']*?"baseUrl":"[^"']*claude-code-releases"[^']*?\})'\)`)

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

// --- Processes --------------------------------------------------------------------------

// Process is one running process.
type Process struct {
	PID, PPID int
	Command   string
}

// processes are the machine's processes, as ps lists them.
func processes() []Process {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil
	}
	var ps []Process
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		rest := strings.TrimLeft(line, " \t")
		for range 2 { // past the two numbers, to the command line as ps printed it
			rest = strings.TrimLeft(rest[strings.IndexAny(rest, " \t"):], " \t")
		}
		ps = append(ps, Process{PID: pid, PPID: ppid, Command: strings.TrimRight(rest, "\n")})
	}
	return ps
}

// descendants are pid's processes' process tree, its children first.
func descendants(pid int) []Process {
	all := processes()
	var out []Process
	frontier := []int{pid}
	for len(frontier) > 0 {
		var next []int
		for _, p := range all {
			if slices.Contains(frontier, p.PPID) {
				out = append(out, p)
				next = append(next, p.PID)
			}
		}
		frontier = next
	}
	return out
}

// ancestry is the chain of processes from pid up to (not including) top, pid first, each its
// command line.
func ancestry(pid, top int) []string {
	byPID := map[int]Process{}
	for _, p := range processes() {
		byPID[p.PID] = p
	}
	var out []string
	for p, ok := byPID[pid]; ok && p.PID != top && len(out) < 16; p, ok = byPID[p.PPID] {
		out = append(out, p.Command)
	}
	return out
}

// procArgs is the command line and environment process pid was started with (sysctl
// KERN_PROCARGS2), as the kernel keeps them: a process of this user's alone.
func procArgs(pid int) (argv, env []string, err error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, nil, err
	}
	if len(data) < 4 {
		return nil, nil, errors.New("procargs2: too short")
	}
	argc := int(binary.LittleEndian.Uint32(data[:4]))
	rest := data[4:]
	// The executable's path, then NULs to align, then argv and the environment.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, nil, errors.New("procargs2: no executable path")
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	for s := range bytes.SplitSeq(rest, []byte{0}) {
		switch {
		case len(argv) < argc:
			argv = append(argv, string(s))
		case len(s) == 0:
			return argv, env, nil
		default:
			env = append(env, string(s))
		}
	}
	return argv, env, nil
}

// --- Machine-wide state the apps read ---------------------------------------------------

// desktopCmd is a command the desktop profiles run of the machine's (sudo, open, osascript),
// ended after 90 seconds: one that waits on a dialog nobody answers would hold the run.
func desktopCmd(name string, args ...string) *exec.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	time.AfterFunc(90*time.Second, cancel)
	return exec.CommandContext(ctx, name, args...)
}

// sudoWrite writes data to path as root, and removes it with t.
func sudoWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	cmd := desktopCmd("sudo", "-n", "sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1" && chmod 644 "$1"`, "sh", path)
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write %s: %v: %s", path, err, out)
	}
	t.Cleanup(func() { _ = desktopCmd("sudo", "-n", "rm", "-f", path).Run() })
}

// trustProxyCA has the machine trust the recording proxy's CA, as a company's TLS-inspecting
// proxy has its own trusted, so an app's own requests are read rather than refused; t takes
// it back.
func trustProxyCA(t *testing.T, p *EgressProxy) {
	t.Helper()
	const keychain = "/Library/Keychains/System.keychain"
	if out, err := desktopCmd("sudo", "-n", "security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", keychain, p.CAFile).CombinedOutput(); err != nil {
		t.Fatalf("trust the proxy's CA: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = desktopCmd("sudo", "-n", "security", "remove-trusted-cert", "-d", p.CAFile).Run()
		_ = desktopCmd("sudo", "-n", "security", "delete-certificate", "-c", egressCAName, keychain).Run()
	})
}

// plistXML is a property list of string and boolean values.
func plistXML(values map[string]any) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n" +
		`<plist version="1.0">` + "\n<dict>\n")
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range values {
			if !yield(k) {
				return
			}
		}
	}) {
		b.WriteString("\t<key>")
		_ = xml.EscapeText(&b, []byte(k))
		b.WriteString("</key>\n")
		switch v := values[k].(type) {
		case bool:
			if v {
				b.WriteString("\t<true/>\n")
			} else {
				b.WriteString("\t<false/>\n")
			}
		default:
			b.WriteString("\t<string>")
			_ = xml.EscapeText(&b, []byte(fmt.Sprint(v)))
			b.WriteString("</string>\n")
		}
	}
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

// --- What a profile keeps of an app ------------------------------------------------------

// DesktopFacts are what a desktop app's profile keeps of its bundle and of the agent it runs:
// the agent's executable facts, and beside them the app's.
type DesktopFacts struct {
	BinaryFacts
	App   AppFacts   `json:"app"`
	Agent AgentFacts `json:"agent"`
}

// AppFacts are the app bundle's identity and signature.
type AppFacts struct {
	Name      string   `json:"name"`
	BundleID  string   `json:"bundle_id"`
	Version   string   `json:"version"`
	Build     string   `json:"build,omitempty"`
	Download  string   `json:"download"`
	SizeBytes int64    `json:"size_bytes"`
	Signing   *Signing `json:"signing,omitempty"`
}

// AgentFacts are the agent the app runs: which harness and build, where the app keeps it, and
// how it comes by it.
type AgentFacts struct {
	Harness string `json:"harness"`
	Version string `json:"version"`
	// Path is where the app runs it from, its placeholders in place.
	Path string `json:"path"`
	// Source says how the app comes by it: "bundled" in the app, or "downloaded" (Claude
	// Desktop's, from Download, on first use, checked against Checksum).
	Source   string `json:"source"`
	Download string `json:"download,omitempty"`
	Checksum string `json:"checksum,omitempty"`
	// SDK is the library the app drives it with, where it names one ("@anthropic-ai/claude-agent-sdk 0.3.317").
	SDK string `json:"sdk,omitempty"`
}

// Launch is how the app started its agent: the command line and environment the agent's
// process was started with, read from the kernel, and the processes between it and the app.
type Launch struct {
	Argv []string `json:"argv"`
	// Env are the names of the variables the app set for its agent: those the agent was started
	// with that neither the app's own environment nor its login shell's has with the same value.
	// EnvValues are their values, a secret's redacted, a list of folders <path>; Inherited how
	// many more it passed on as it had them.
	Env       []string          `json:"env"`
	EnvValues map[string]string `json:"env_values,omitempty"`
	Inherited int               `json:"inherited"`
	// Parents are the commands between the app and the agent, the agent's parent first.
	Parents []string `json:"parents,omitempty"`
}

// loginEnv is the environment a login session's launchd gives the apps it starts, the Finder's
// and the Dock's: the user's identity and home, their login shell, the system PATH, and the
// session's own sockets. A runner's or a developer's shell exports none of it.
func loginEnv() []string {
	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "SHELL=" + loginShell()}
	for _, k := range []string{"HOME", "USER", "LOGNAME", "TMPDIR", "SSH_AUTH_SOCK", "__CF_USER_TEXT_ENCODING"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// loginShell is the user's login shell, as the user database has it.
func loginShell() string {
	out, err := exec.Command("dscl", ".", "-read", "/Users/"+os.Getenv("USER"), "UserShell").Output()
	if _, shell, ok := strings.Cut(strings.TrimSpace(string(out)), "UserShell: "); err == nil && ok && shell != "" {
		return shell
	}
	return "/bin/zsh"
}

// loginNote says what login shell the app's user had, which the app's agent takes its
// environment from: a new Mac user's (zsh, no startup files) or the machine's own.
func loginNote() string {
	shell := loginShell()
	home, _ := os.UserHomeDir()
	var rc []string
	for _, f := range []string{".bash_profile", ".bashrc", ".profile", ".zshrc", ".zprofile", ".zshenv", ".zlogin"} {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			rc = append(rc, f)
		}
	}
	if shell == "/bin/zsh" && len(rc) == 0 {
		return "The machine's user was a new Mac user: zsh, the macOS default, its login shell, and no shell startup files of its own."
	}
	return "The machine's user's login shell was " + shell + ", with its startup files (" + strings.Join(rc, ", ") + "): the app's agent took its environment from them."
}

// loginShellEnv is what the user's login shell exports, started as an app resolves it (Claude
// Desktop gives its agent the login shell's environment): the machine's, not the app's.
func loginShellEnv() map[string]string {
	cmd := desktopCmd(loginShell(), "-l", "-c", "env")
	cmd.Env = loginEnv()
	out, _ := cmd.Output()
	env := map[string]string{}
	for line := range strings.Lines(string(out)) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
			env[k] = v
		}
	}
	return env
}

// sessionEnv are the variables any app has from the login session (launchd's), whose values
// are the machine's, not the app's choice.
var sessionEnv = map[string]bool{"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true, "PATH": true,
	"SSH_AUTH_SOCK": true, "__CF_USER_TEXT_ENCODING": true, "XPC_FLAGS": true, "XPC_SERVICE_NAME": true, "LANG": true,
	"__CFBundleIdentifier": true, "COMMAND_MODE": true, "PWD": true, "OLDPWD": true, "SHLVL": true, "_": true,
	"MallocNanoZone": true, "LC_ALL": true, "LC_CTYPE": true, "TERM": true, "TERM_PROGRAM": true}

// machineNames is v with the machine's own names in it placeholders, as an app writes them into
// a value of its own (Claude Desktop's OTEL_RESOURCE_ATTRIBUTES: process.owner, os.version):
// the user's name <user>, the OS's release <os-release>.
func machineNames(v string) string {
	if user := os.Getenv("USER"); len(user) >= 3 {
		v = regexp.MustCompile(`\b`+regexp.QuoteMeta(user)+`\b`).ReplaceAllString(v, "<user>")
	}
	return osVersionAttr.ReplaceAllString(v, "${1}<os-release>")
}

// osVersionAttr is an OS release as a resource attribute names it.
var osVersionAttr = regexp.MustCompile(`((?:^|[,;\s])os\.version=)[0-9][0-9.]*`)

// runKey is a value that is a key made for one run: base64 of 32 bytes or more.
var runKey = regexp.MustCompile(`^[A-Za-z0-9+/]{40,}={0,2}$`)

// launchOf is how pid was started, below the app at top: what the app set of its environment
// (neither the app's own environment, read from the kernel, nor its login shell's), its values
// scrubbed as a hook's variables are (a secret's by its name or shape redacted, a list of folders
// <path>).
func launchOf(pid, top int) (Launch, error) {
	argv, env, err := procArgs(pid)
	if err != nil {
		return Launch{}, err
	}
	had := loginShellEnv()
	if _, appEnv, err := procArgs(top); err == nil {
		for _, kv := range appEnv {
			k, v, _ := strings.Cut(kv, "=")
			had[k] = v
		}
	}
	l := Launch{Argv: argv, EnvValues: map[string]string{}}
	if chain := ancestry(pid, top); len(chain) > 1 {
		l.Parents = chain[1:]
	}
	for i, a := range l.Argv {
		if secretValueRE.MatchString(a) {
			l.Argv[i] = "<redacted>"
		}
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if was, ok := had[k]; ok && was == v || sessionEnv[k] && !ok {
			l.Inherited++
			continue
		}
		l.Env = append(l.Env, k)
		switch {
		case v != "" && (secretName(k) || secretValueRE.MatchString(v)):
			l.EnvValues[k] = "<redacted>"
		case pathList(v):
			l.EnvValues[k] = "<path>"
		case sessionEnv[k]:
			l.EnvValues[k] = "<session>" // the app changed it: to what is the machine's
		case runKey.MatchString(v):
			l.EnvValues[k] = "<key>" // a key the app made for this run (a public key, base64)
		default:
			l.EnvValues[k] = machineNames(v)
		}
	}
	sort.Strings(l.Env)
	l.Env = slices.Compact(l.Env)
	return l, nil
}

// RecordLaunch records how the app launched its agent, as the "launch" probe.
func RecordLaunch(t *testing.T, b Binary, l Launch) {
	t.Helper()
	RecordProbe(t, b, "launch", l)
}

// egressCAName is the recording proxy's CA's name, as a keychain finds it.
const egressCAName = "terma e2e egress CA"
