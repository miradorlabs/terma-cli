package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

func TestAutomaticUpdatePreference(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", dir)
	for _, step := range []struct {
		args []string
		want string
	}{
		{[]string{"update", "--auto", "status"}, "off (notify only)"},
		{[]string{"update", "--auto", "on"}, "enabled"},
		{[]string{"update", "--auto", "status"}, "on"},
		{[]string{"update", "--auto", "off"}, "disabled"},
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
	if err != nil || p.Auto {
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
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
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
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
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
