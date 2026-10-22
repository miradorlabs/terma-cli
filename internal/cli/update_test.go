package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

func TestAutomaticUpdatePreference(t *testing.T) {
	dir := t.TempDir()
	useConfigDir(t, dir)
	for _, step := range []struct {
		args []string
		want string
	}{
		{[]string{"update", "--auto", "status"}, "Automatic updates: on."},
		{[]string{"update", "--auto", "off"}, "disabled"},
		{[]string{"update", "--auto", "status"}, "off (notify only)"},
		{[]string{"update", "--auto", "on"}, "enabled"},
		{[]string{"update", "--auto", "status"}, "Automatic updates: on."},
	} {
		out, err := runTerma(t, step.args...)
		if err != nil || !strings.Contains(out, step.want) {
			t.Fatalf("%v: %v, %s", step.args, err, out)
		}
	}
	for _, args := range [][]string{{"update", "--auto", "bad"}, {"update", "--auto", "on", "--check"}, {"update", "--auto", "on", "--force"}} {
		if _, err := runTerma(t, args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	p, err := selfupdate.LoadPreferences(dir)
	if err != nil || !p.Auto {
		t.Fatalf("invalid invocation changed preference: %+v %v", p, err)
	}
}

func TestAutomaticUpdateCommandEligibility(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("TERMA_NO_UPDATE_CHECK", "")
	for _, name := range []string{"hook", "spool", "update", "version", "completion"} {
		root := testApp.NewRootCommand()
		root.InitDefaultCompletionCmd()
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		if testApp.automaticUpdatesAllowed(cmd, true) {
			t.Fatalf("updates allowed in %s", name)
		}
	}
	root := testApp.NewRootCommand()
	cmd, _, _ := root.Find([]string{"status"})
	if !testApp.automaticUpdatesAllowed(cmd, true) || testApp.automaticUpdatesAllowed(cmd, false) {
		t.Fatal("interactive eligibility wrong")
	}
	t.Setenv("CI", "1")
	if testApp.automaticUpdatesAllowed(cmd, true) {
		t.Fatal("CI enabled updates")
	}
	t.Setenv("CI", "")
	t.Setenv("TERMA_NO_UPDATE_CHECK", "1")
	if testApp.automaticUpdatesAllowed(cmd, true) {
		t.Fatal("opt-out ignored")
	}
	t.Setenv("TERMA_NO_UPDATE_CHECK", "")
	if err := root.PersistentFlags().Set("output", "json"); err != nil {
		t.Fatal(err)
	}
	if testApp.automaticUpdatesAllowed(cmd, true) {
		t.Fatal("JSON enabled updates")
	}
}

func TestUpdateCheckAndSourceBuildGuard(t *testing.T) {
	for _, version := range []string{"1.0.0", "132b086", "dev"} {
		t.Run(version, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++

				if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
					t.Fatalf("unexpected download %s", r.URL.Path)
				}
				fmt.Fprint(w, `{"tag_name":"v2.0.0","assets":[]}`)
			}))
			defer srv.Close()
			c := &selfupdate.Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: version}
			dir := t.TempDir()
			exe := filepath.Join(t.TempDir(), "terma")
			if err := os.WriteFile(exe, []byte("keep me"), 0755); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if _, err := testApp.runUpdate(context.Background(), c, dir, exe, &out, true, false); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("check made %d calls", calls)
			}
			data, _ := os.ReadFile(exe)
			if string(data) != "keep me" {
				t.Fatal("--check changed binary")
			}
			if selfupdate.LoadCache(dir).CheckedAt.IsZero() {
				t.Fatal("check cache timestamp missing")
			}

			if version != "1.0.0" {
				// A source build is left in place (update then only refreshes) and told about --force.
				out.Reset()
				installed, err := testApp.runUpdate(context.Background(), c, dir, exe, &out, false, false)
				if err != nil || installed || !strings.Contains(out.String(), "--force") {
					t.Fatalf("source build not guarded: installed %v, err %v, output %q", installed, err, &out)
				}
				if data, _ := os.ReadFile(exe); string(data) != "keep me" {
					t.Fatal("update replaced a source build without --force")
				}
			}
		})
	}
}

func TestUpdateCheckExplainsMissingRelease(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := &selfupdate.Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
	var out bytes.Buffer
	dir := t.TempDir()
	if _, err := testApp.runUpdate(context.Background(), c, dir, "unused", &out, true, false); err != nil {
		t.Fatal(err)
	}
	if !selfupdate.LoadCache(dir).Failed {
		t.Fatal("missing release must use the failure retry interval")
	}
	if !strings.Contains(out.String(), "No binary update is available") {
		t.Fatal(out.String())
	}
}

// Already on the latest release, `terma update` is the refresh, so it is safe to run again.
func TestUpdateOnTheLatestReleaseRefreshes(t *testing.T) {
	useConfigDir(t, t.TempDir())
	sandboxMachine(t)
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.0.0","assets":[]}`)
	}))
	defer srv.Close()
	c := &selfupdate.Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
	var out bytes.Buffer
	if err := testApp.updateOrRefresh(context.Background(), c, t.TempDir(), "unused", &out, false, false); err != nil {
		t.Fatalf("update: %v\n%s", err, &out)
	}
	for _, want := range []string{"is up to date", "refresh"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output should say %q:\n%s", want, &out)
		}
	}

	// --check only reports.
	out.Reset()
	if err := testApp.updateOrRefresh(context.Background(), c, t.TempDir(), "unused", &out, true, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "efresh") {
		t.Errorf("--check refreshed:\n%s", &out)
	}
}

// A failed check refreshes nothing: the error is the whole answer.
func TestUpdateThatCannotCheckRefreshesNothing(t *testing.T) {
	useConfigDir(t, t.TempDir())
	sandboxMachine(t)
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := &selfupdate.Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
	var out bytes.Buffer
	if err := testApp.updateOrRefresh(context.Background(), c, t.TempDir(), "unused", &out, false, false); err == nil {
		t.Fatalf("update without a release should fail:\n%s", &out)
	}
	if strings.Contains(out.String(), "efresh") {
		t.Errorf("a failed update refreshed:\n%s", &out)
	}
}

// Setup says how this installation gets new releases: by itself, through the package
// manager that owns it in the background, through that manager by hand when terma cannot
// run it, or not at all.
func TestUpdatesSummary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix package-manager layouts")
	}
	t.Setenv("PATH", "")
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brew, npm := filepath.Join(home, "brew"), filepath.Join(home, "node")
	if err := os.Mkdir(filepath.Join(home, "locked"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, "locked"), 0o755) })
	for _, f := range []string{filepath.Join(brew, "bin", "brew"), filepath.Join(npm, "bin", "npm")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, exe, version, goos string
		auto                     bool
		want                     string
	}{
		{"installer script", filepath.Join(home, ".local", "bin", "terma"), "1.2.0", "darwin", true, "automatically: terma installs"},
		{"turned off", filepath.Join(home, ".local", "bin", "terma"), "1.2.0", "linux", false, "`terma update --auto on`"},
		{"homebrew", filepath.Join(brew, "Caskroom", "terma", "1.2.0", "terma"), "1.2.0", "darwin", true, "the local relay has Homebrew install each new release"},
		{"homebrew, turned off", filepath.Join(brew, "Caskroom", "terma", "1.2.0", "terma"), "1.2.0", "darwin", false, "`terma update --auto on`"},
		{"homebrew not found", filepath.Join(home, "Caskroom", "terma", "1.2.0", "terma"), "1.2.0", "darwin", true, "through Homebrew: `brew upgrade terma`"},
		{"npm", filepath.Join(npm, "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "1.2.0", "linux", true, "the local relay has npm install each new release"},
		{"npm not found", filepath.Join(home, "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "1.2.0", "linux", true, "through npm: `npm install -g @miradorlabs/terma@latest`"},
		{"npm project", filepath.Join(home, "app", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "1.2.0", "linux", true, "through npm: `npm install @miradorlabs/terma@latest`"},
		{"source build", filepath.Join(home, "go", "bin", "terma"), "v1.2.0-3-g401af35", "darwin", true, "never for a development build"},
		{"windows", filepath.Join(home, "terma.exe"), "1.2.0", "windows", true, "download each new release"},
		{"installed where it may not write", filepath.Join(home, "locked", "terma"), "1.2.0", "linux", true, "`terma update` as a user who may write in " + filepath.Join(home, "locked")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.exe, "locked") && os.Geteuid() == 0 {
				t.Skip("root may write anywhere")
			}
			if got := updatesSummary(selfupdate.InstallOn(tc.exe, tc.version, tc.goos), tc.auto); !strings.Contains(got, tc.want) {
				t.Fatalf("updatesSummary = %q, want it to say %q", got, tc.want)
			}
		})
	}
}

// Every updater this process runs knows the executable it started from, captured as the
// process starts: a command that ran a while, or the relay, may outlive its version.
func TestTheUpdaterKnowsTheBinaryTheProcessStartedFrom(t *testing.T) {
	app := New(builtin.Agents, "1.2.0")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	now, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if c := app.updateClient(); c.Binary == nil || !os.SameFile(c.Binary, now) || c.Version != "1.2.0" {
		t.Fatalf("updateClient = %+v, want this executable at 1.2.0", c)
	}
}

// `terma update` from a process whose binary another install has replaced meanwhile does
// nothing but say so: its version no longer says what is installed.
func TestUpdateRefusesOnceAnotherInstallReplacedTerma(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		fmt.Fprint(w, `{"tag_name":"v2.0.0","assets":[]}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	exe := filepath.Join(dir, "terma")
	if err := os.WriteFile(exe, []byte("1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	started, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(dir, "terma.next")
	if err := os.WriteFile(next, []byte("1.0.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, exe); err != nil {
		t.Fatal(err)
	}
	c := &selfupdate.Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0", Binary: started}
	var out bytes.Buffer
	installed, err := testApp.runUpdate(context.Background(), c, t.TempDir(), exe, &out, false, false)
	if err == nil || !strings.Contains(err.Error(), "replaced terma meanwhile") || installed || calls != 0 {
		t.Fatalf("runUpdate = %v, %v after %d lookups; want a refusal and no lookup", installed, err, calls)
	}
	if got, _ := os.ReadFile(exe); string(got) != "1.0.1" {
		t.Fatalf("the installed binary is now %q", got)
	}
}

// `terma update` on a release GitHub already names the latest, but whose build has not
// uploaded its files yet, says the release is still being built, and leaves the binary alone;
// so does --check, rather than call it available. Both record it as unbuilt, as the
// background look does, so no later command announces it.
func TestUpdateSaysWhenTheLatestReleaseIsStillBeingBuilt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("in-place updates are Unix-only")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.0.1","assets":[]}`)
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "terma")
	if err := os.WriteFile(exe, []byte("1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &selfupdate.Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
	var out bytes.Buffer
	stateDir := t.TempDir()
	if _, err := testApp.runUpdate(context.Background(), c, stateDir, exe, &out, true, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "still being built") || strings.Contains(out.String(), "available") {
		t.Fatalf("--check said %q of a release with no files yet", &out)
	}
	if cache := selfupdate.LoadCache(stateDir); !cache.Unbuilt || cache.Latest != "1.0.1" {
		t.Fatalf("--check recorded %+v, want 1.0.1 unbuilt", cache)
	}
	installed, err := testApp.runUpdate(context.Background(), c, stateDir, exe, &out, false, false)
	if !errors.Is(err, selfupdate.ErrNotReady) || !strings.Contains(err.Error(), "still being built") || installed {
		t.Fatalf("runUpdate = %v, %v; want the release named as still being built", installed, err)
	}
	if !selfupdate.LoadCache(stateDir).Unbuilt {
		t.Fatal("update recorded a release with no files as built")
	}
	// A package manager is not asked for it either: its package comes from the same build.
	managed := filepath.Join(t.TempDir(), "Cellar", "terma", "1.0.0", "bin", "terma")
	if err := os.MkdirAll(filepath.Dir(managed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managed, []byte("1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if installed, err := testApp.runUpdate(context.Background(), c, stateDir, managed, &out, false, false); !errors.Is(err, selfupdate.ErrNotReady) || installed {
		t.Fatalf("runUpdate of a Homebrew terma = %v, %v; want the release named as still being built", installed, err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "1.0.0" {
		t.Fatalf("the installed binary is now %q", got)
	}
}

// `terma update` and --check keep the record of the last install tried when they record
// their own look: a release the background update holds back after a failed install is not
// tried again at its next pass because a developer checked meanwhile.
func TestUpdateKeepsTheLastInstallTried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.0.1","assets":[]}`)
	}))
	defer srv.Close()
	stateDir := t.TempDir()
	tried := time.Now().Add(-time.Hour).Round(time.Second)
	selfupdate.SaveCache(stateDir, selfupdate.Cache{CheckedAt: tried, Current: "1.0.0", Latest: "1.0.1", AttemptAt: tried, Attempted: "1.0.1", AttemptedBy: "1.0.0"})
	c := &selfupdate.Client{BaseURL: srv.URL, HTTP: srv.Client(), Version: "1.0.0"}
	var out bytes.Buffer
	if _, err := testApp.runUpdate(context.Background(), c, stateDir, "unused", &out, true, false); err != nil {
		t.Fatal(err)
	}
	cache := selfupdate.LoadCache(stateDir)
	if cache.Attempted != "1.0.1" || cache.AttemptedBy != "1.0.0" || !cache.AttemptAt.Equal(tried) || !cache.CheckedAt.After(tried) {
		t.Fatalf("--check recorded %+v, want its own look and the install tried an hour ago kept", cache)
	}
}

// The relay updates itself only as a release it could replace, outside CI and the opt-out,
// exactly when its heartbeat says terma.update.auto on; otherwise it asks GitHub nothing.
func TestTheRelayUpdatesOnlyWhereItCould(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a Windows relay never replaces itself")
	}
	t.Setenv("CI", "")
	t.Setenv("TERMA_NO_UPDATE_CHECK", "")
	release := New(builtin.Agents, "1.2.0")
	release.dir, release.stateDir = t.TempDir(), t.TempDir()
	install := release.installed()
	if install.Method != semconv.TermaInstallMethodScript || release.updateAuto(install) != semconv.TermaUpdateAutoOn {
		t.Fatalf("a release binary of its own is %s, auto %s", install.Method, release.updateAuto(install))
	}
	if release.relayUpdater(install) == nil {
		t.Fatal("a release relay does not update itself")
	}
	dev := testApp.installed()
	if dev.Method != semconv.TermaInstallMethodSource || testApp.updateAuto(dev) != semconv.TermaUpdateAutoUnsupported || testApp.relayUpdater(dev) != nil {
		t.Fatalf("a development build's relay updates itself: %s, auto %s", dev.Method, testApp.updateAuto(dev))
	}
	// A pass that looked would have recorded its check.
	looked := func() bool { return !selfupdate.LoadCache(release.stateDir).CheckedAt.IsZero() }
	for _, env := range []string{"CI", "TERMA_NO_UPDATE_CHECK"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "1")
			if got := release.updateAuto(install); got != semconv.TermaUpdateAutoOff {
				t.Fatalf("under %s terma.update.auto = %q", env, got)
			}
			if v, err := release.relayUpdater(install).Update(t.Context()); v != "" || err != nil || looked() {
				t.Fatalf("under %s the relay's pass = %q, %v, looked %v", env, v, err, looked())
			}
		})
	}
	updater := release.relayUpdater(install)
	if err := selfupdate.SavePreferences(release.dir, selfupdate.Preferences{Auto: false}); err != nil {
		t.Fatal(err)
	}
	if got := release.updateAuto(install); got != semconv.TermaUpdateAutoOff {
		t.Fatalf("turned off, terma.update.auto = %q", got)
	}
	// The relay already running reads the choice at each pass, as its heartbeat does.
	if v, err := updater.Update(t.Context()); v != "" || err != nil || looked() {
		t.Fatalf("turned off, the relay's pass = %q, %v, looked %v", v, err, looked())
	}
	if err := selfupdate.SavePreferences(release.dir, selfupdate.Preferences{Auto: true}); err != nil {
		t.Fatal(err)
	}
	// A Homebrew install updates through the brew that owns it, and only where there is one.
	prefix := t.TempDir()
	cask := filepath.Join(prefix, "Caskroom", "terma", "1.2.0", "terma")
	if brew := selfupdate.InstallOf(cask, "1.2.0"); release.relayUpdater(brew) != nil ||
		release.updateAuto(brew) != semconv.TermaUpdateAutoUnsupported {
		t.Fatalf("a Homebrew install without its brew updates itself: auto %s", release.updateAuto(brew))
	}
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "bin", "brew"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if brew := selfupdate.InstallOf(cask, "1.2.0"); brew.Method != semconv.TermaInstallMethodHomebrew ||
		release.updateAuto(brew) != semconv.TermaUpdateAutoOn || release.relayUpdater(brew) == nil {
		t.Fatalf("a Homebrew install with its brew is %s, auto %s", brew.Method, release.updateAuto(brew))
	}
}

// A relay that updated itself has the new release refresh what terma installed, then, when
// a hook started it, starts its successor, since no service will: both from where its
// install's new release is found, a package manager's link rather than the versioned path
// it ran from. A service's relay is restarted by its service manager, and one that made way
// for a newer hook's terma is started again by that hook.
func TestOnlyAnUpdatedHookStartedRelayStartsItsSuccessor(t *testing.T) {
	script := selfupdate.InstallOn("/home/d/.local/bin/terma", "1.3.0", "linux")
	brew := selfupdate.InstallOn("/home/linuxbrew/.linuxbrew/Caskroom/terma/1.3.0/terma", "1.3.0", "linux")
	for _, tc := range []struct {
		name      string
		res       daemon.Result
		install   selfupdate.Install
		from      string
		spawned   bool
		refreshed bool
		restart   bool
	}{
		{"updated, hook-started", daemon.Result{Updated: true}, script, "/home/d/.local/bin/terma", true, true, true},
		{"updated through Homebrew, hook-started", daemon.Result{Updated: true}, brew, "/home/linuxbrew/.linuxbrew/bin/terma", true, true, true},
		{"updated, service", daemon.Result{Updated: true, Service: true}, brew, "/home/linuxbrew/.linuxbrew/bin/terma", false, true, true},
		{"updated, then torn down", daemon.Result{Updated: true, SetupGone: true}, script, "", false, false, true},
		{"replaced, hook-started", daemon.Result{Replaced: true}, script, "", false, false, true},
		{"idle, hook-started", daemon.Result{}, script, "", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := New(builtin.Agents, "1.3.0")
			app.stateDir = t.TempDir()
			spawned, refreshed := false, false
			app.spawnRelay = func(exe, stateDir, version string) {
				spawned = exe == tc.from && stateDir == app.stateDir && version == "1.3.0"
			}
			app.backgroundStep = func(_ context.Context, _ io.Writer, argv ...string) error {
				refreshed = slices.Equal(argv, []string{tc.from, "update", "--refresh"})
				return nil
			}
			err := app.afterRelay(tc.res, tc.install, io.Discard)
			if spawned != tc.spawned || refreshed != tc.refreshed || (err != nil) != tc.restart {
				t.Fatalf("afterRelay(%+v): spawned %v, refreshed %v, err %v; want spawned %v, refreshed %v, restart %v",
					tc.res, spawned, refreshed, err, tc.spawned, tc.refreshed, tc.restart)
			}
		})
	}
}

// doctor says how this terma updates, what it last installed by itself, which a relay only
// logs, and why its last automatic install of a newer release failed, until it is retried.
func TestDoctorShowsTheLastAutomaticUpdate(t *testing.T) {
	app := New(builtin.Agents, "1.2.0")
	app.dir, app.stateDir = t.TempDir(), t.TempDir()
	exe := filepath.Join(app.stateDir, "bin", "terma")
	if rows := app.updateRows(exe); len(rows) != 1 || rows[0].Label != "Updates" || !strings.Contains(rows[0].Value, "automatically") {
		t.Fatalf("rows before any update: %+v", rows)
	}
	updated := filepath.Join(app.stateDir, selfupdate.Dir, "updated.json")
	if err := os.MkdirAll(filepath.Dir(updated), 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 22, 9, 30, 0, 0, time.Local)
	if err := os.WriteFile(updated, fmt.Appendf(nil, `{"at":%q,"from":"1.1.9","to":"1.2.0","through":"Homebrew"}`, at.Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}
	selfupdate.SaveCache(app.stateDir, selfupdate.Cache{CheckedAt: time.Now(), Current: "1.2.0", Latest: "1.2.1",
		AttemptAt: at, Attempted: "1.2.1", AttemptedBy: "1.2.0", Failure: "Homebrew could not upgrade terma to 1.2.1 (exit status 1)"})
	rows := app.updateRows(exe)
	if len(rows) != 3 || rows[1].Label != "Last update" || rows[1].Value != "1.1.9 → 1.2.0 automatically (Homebrew), 2026-10-22 09:30" {
		t.Fatalf("rows: %+v", rows)
	}
	if want := "installing 1.2.1 failed 2026-10-22 09:30: Homebrew could not upgrade terma to 1.2.1 (exit status 1); tried again from 2026-10-23 09:30"; rows[2] != (doctor.Row{Value: want}) {
		t.Fatalf("failure row %+v, want %q", rows[2], want)
	}
}
