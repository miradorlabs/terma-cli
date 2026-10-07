package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
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

// Setup says how this installation gets new releases: through the package manager that
// owns it, by itself, or not at all.
func TestUpdatesSummary(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct {
		name, exe, version, goos string
		auto                     bool
		want                     string
	}{
		{"installer script", filepath.Join(home, ".local", "bin", "terma"), "1.2.0", "darwin", true, "automatically"},
		{"turned off", filepath.Join(home, ".local", "bin", "terma"), "1.2.0", "linux", false, "`terma update --auto on`"},
		{"homebrew", filepath.Join(home, "Caskroom", "terma", "1.2.0", "terma"), "1.2.0", "darwin", true, "through Homebrew: `brew upgrade terma`"},
		{"npm", filepath.Join(home, "lib", "node_modules", "@miradorlabs", "terma", "vendor", "terma"), "1.2.0", "linux", true, "through npm: `npm install -g @miradorlabs/terma@latest`"},
		{"source build", filepath.Join(home, "go", "bin", "terma"), "v1.2.0-3-g401af35", "darwin", true, "never for a development build"},
		{"windows", filepath.Join(home, "terma.exe"), "1.2.0", "windows", true, "download each new release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := updatesSummary(tc.exe, tc.version, tc.goos, tc.auto); !strings.Contains(got, tc.want) {
				t.Fatalf("updatesSummary = %q, want it to say %q", got, tc.want)
			}
		})
	}
}

// The relay updates itself only as a release it could replace, outside CI and the opt-out;
// turned off with --auto off, it asks GitHub nothing.
func TestTheRelayUpdatesOnlyWhereItCould(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a Windows relay never replaces itself")
	}
	t.Setenv("CI", "")
	t.Setenv("TERMA_NO_UPDATE_CHECK", "")
	release := New(builtin.Agents, "1.2.0")
	release.dir, release.stateDir = t.TempDir(), t.TempDir()
	if release.relayUpdater() == nil {
		t.Fatal("a release relay does not update itself")
	}
	if testApp.relayUpdater() != nil {
		t.Fatal("a development build's relay updates itself")
	}
	for _, env := range []string{"CI", "TERMA_NO_UPDATE_CHECK"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "1")
			if release.relayUpdater() != nil {
				t.Fatalf("the relay updates itself under %s", env)
			}
		})
	}
	if err := selfupdate.SavePreferences(release.dir, selfupdate.Preferences{Auto: false}); err != nil {
		t.Fatal(err)
	}
	if v, err := release.relayUpdater().Update(t.Context()); v != "" || err != nil {
		t.Fatalf("turned off, the relay's pass = %q, %v", v, err)
	}
	// A pass that looked would have recorded its check.
	if checked := !selfupdate.LoadCache(release.stateDir).CheckedAt.IsZero(); checked {
		t.Fatal("turned off, the relay still looked for a release")
	}
}

// A hook-started relay that updated itself starts its successor, since no service will; a
// service's relay is restarted by its service manager, and one that made way for a newer
// hook's terma is started again by that hook.
func TestOnlyAnUpdatedHookStartedRelayStartsItsSuccessor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		res     daemon.Result
		spawned bool
		restart bool
	}{
		{"updated, hook-started", daemon.Result{Updated: true}, true, true},
		{"updated, service", daemon.Result{Updated: true, Service: true}, false, true},
		{"updated, then torn down", daemon.Result{Updated: true, SetupGone: true}, false, true},
		{"replaced, hook-started", daemon.Result{Replaced: true}, false, true},
		{"idle, hook-started", daemon.Result{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := New(builtin.Agents, "1.3.0")
			app.stateDir = t.TempDir()
			spawned := false
			app.spawnRelay = func(stateDir, version string) {
				spawned = stateDir == app.stateDir && version == "1.3.0"
			}
			err := app.afterRelay(tc.res)
			if spawned != tc.spawned || (err != nil) != tc.restart {
				t.Fatalf("afterRelay(%+v): spawned %v, err %v; want spawned %v, restart %v", tc.res, spawned, err, tc.spawned, tc.restart)
			}
		})
	}
}
