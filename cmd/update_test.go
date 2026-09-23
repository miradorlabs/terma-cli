package cmd

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
	for _, name := range []string{"hook", "shim", "spool", "update", "version", "completion"} {
		root := NewRootCommand()
		root.InitDefaultCompletionCmd()
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		if automaticUpdatesAllowed(cmd, true) {
			t.Fatalf("updates allowed in %s", name)
		}
	}
	root := NewRootCommand()
	cmd, _, _ := root.Find([]string{"status"})
	if !automaticUpdatesAllowed(cmd, true) || automaticUpdatesAllowed(cmd, false) {
		t.Fatal("interactive eligibility wrong")
	}
	t.Setenv("CI", "1")
	if automaticUpdatesAllowed(cmd, true) {
		t.Fatal("CI enabled updates")
	}
	t.Setenv("CI", "")
	t.Setenv("TERMA_NO_UPDATE_CHECK", "1")
	if automaticUpdatesAllowed(cmd, true) {
		t.Fatal("opt-out ignored")
	}
	t.Setenv("TERMA_NO_UPDATE_CHECK", "")
	if err := root.PersistentFlags().Set("output", "json"); err != nil {
		t.Fatal(err)
	}
	if automaticUpdatesAllowed(cmd, true) {
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
			if err := runUpdate(context.Background(), c, dir, exe, &out, true, false); err != nil {
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
				err := runUpdate(context.Background(), c, dir, exe, &out, false, false)
				if err == nil || !strings.Contains(err.Error(), "--force") {
					t.Fatalf("source build not guarded: %v", err)
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
	if err := runUpdate(context.Background(), c, dir, "unused", &out, true, false); err != nil {
		t.Fatal(err)
	}
	if !selfupdate.LoadCache(dir).Failed {
		t.Fatal("missing release must use the failure retry interval")
	}
	if !strings.Contains(out.String(), "No binary update is available") {
		t.Fatal(out.String())
	}
}
