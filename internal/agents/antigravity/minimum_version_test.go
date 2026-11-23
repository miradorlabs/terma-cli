package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// fakeAgy puts an agy on PATH whose --version prints out and exits with code.
func fakeAgy(t *testing.T, out string, code int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for agy")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + out + "'\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// An agy older than the first terma relies on is not offered by setup --yes, and setup
// refuses to write its hooks into ~/.gemini, saying why; removal still works. A client's
// home is planned all the same: the client runs an agy of its own, through agy acp.
func TestAntigravityTooOld(t *testing.T) {
	fakeAgy(t, "1.1.9", 0)
	home := isolatedHome(t)
	ctx := context.Background()
	if (Agent{}).Installed(ctx) {
		t.Error("an agy before 1.2.4 counts as installed")
	}
	if v := (Agent{}).DetectedVersion(ctx); v != "1.1.9" {
		t.Errorf("DetectedVersion = %q", v)
	}
	dir := filepath.Join(home, ".gemini", "config")
	if _, err := (Agent{}).PlanUserHooks(dir, testCommand, true); err == nil || !strings.Contains(err.Error(), "1.2.4 or later") {
		t.Fatalf("install for an old agy: %v", err)
	}
	if _, err := (Agent{}).PlanUserHooks(dir, testCommand, false); err != nil {
		t.Fatalf("removal for an old agy: %v", err)
	}
	client := filepath.Join(t3Home(t, filepath.Join(home, ".t3"), "antigravity", "a1"), "config")
	plan, err := (Agent{}).PlanUserHooks(client, testCommand, true)
	if err != nil || plan.Empty() {
		t.Fatalf("install into a client's home with an old agy on PATH: %+v, %v", plan, err)
	}
	// setup --yes would otherwise stop at ~/.gemini's refusal.
	if (Agent{}).Installed(ctx) {
		t.Error("an agy before 1.2.4 counts as installed beside a client's home")
	}
}

// An agy at or past the floor is offered; one whose version cannot be read is too, and
// doctor names its version as unknown.
func TestAntigravityVersionProbe(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		out     string
		code    int
		version string
	}{{"1.2.4", 0, "1.2.4"}, {"agy 1.5.15", 0, "1.5.15"}, {"broken", 1, ""}} {
		fakeAgy(t, tc.out, tc.code)
		if !(Agent{}).Installed(ctx) {
			t.Errorf("%q: not installed", tc.out)
		}
		if v := (Agent{}).DetectedVersion(ctx); v != tc.version {
			t.Errorf("%q: DetectedVersion = %q, want %q", tc.out, v, tc.version)
		}
		if _, err := (Agent{}).PlanUserHooks(t.TempDir(), testCommand, true); err != nil {
			t.Errorf("%q: install refused: %v", tc.out, err)
		}
	}
}

func TestParseVersion(t *testing.T) {
	t.Parallel()
	for in, want := range map[string][]int{"1.2.4": {1, 2, 4}, "v1.5": {1, 5, 0}, "1.5.15-beta+x": {1, 5, 15}, "2": {2, 0, 0}} {
		if got, ok := parseVersion(in); !ok || !slices.Equal(got, want) {
			t.Errorf("parseVersion(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	if _, ok := parseVersion("agy"); ok {
		t.Error("parsed a version from no number")
	}
}

// After terma moves, a refresh points the hooks it wrote at the terma it runs as; a file
// without terma's hooks is never written.
func TestAntigravityRefreshHooks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	plan, err := planUserHooks(dir, hookmgr.UserHookCommand("/old/terma"), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(dir, plan); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := refreshHooks(path, "/new/terma"); err != nil || !changed {
		t.Fatalf("refresh = %v, %v", changed, err)
	}
	got := hookruntest.ReadFile(t, dir, "hooks.json")
	if strings.Contains(got, "/old/terma") || !strings.Contains(got, "/new/terma") {
		t.Fatalf("after refresh:\n%s", got)
	}
	if _, changed, err := refreshHooks(path, "/new/terma"); err != nil || changed {
		t.Fatalf("a second refresh = %v, %v", changed, err)
	}
}
