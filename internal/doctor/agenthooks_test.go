package doctor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// versioned is a hooks-only agent that reports its installed version.
type versioned struct {
	agentstest.UserHooked
	version string
}

func (v versioned) DetectedVersion(context.Context) string { return v.version }

// binary writes an executable stand-in for terma at path.
func binary(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// An agent that reports through its hooks alone is recorded only while they are there:
// doctor fails a chosen one whose hooks are gone or name a terma that is not there, accepts
// another path to this same terma, warns of hooks that run another terma, and says nothing
// of one the developer did not choose.
func TestAgentHooksCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	fake := versioned{agentstest.UserHooked{Agent: agentstest.Agent{ID: "fake"}, Path: path}, "1.5.15"}
	reg := agents.New(fake).Support(fake)
	exe := binary(t, filepath.Join(t.TempDir(), "bin", "terma"))
	install := func(terma string) {
		t.Helper()
		plan, err := fake.PlanUserHooks(dir, hookmgr.UserHookCommand(terma), true)
		if err != nil {
			t.Fatal(err)
		}
		if err := hookmgr.Apply(dir, plan); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want Status, contains string) {
		t.Helper()
		c, ok := AgentHooksCheck(ctx, reg, []string{"fake"}, exe)
		if !ok || c.Status != want || !strings.Contains(c.Detail, contains) {
			t.Fatalf("check = %+v, %v; want %v naming %q", c, ok, want, contains)
		}
	}

	if _, ok := AgentHooksCheck(ctx, reg, nil, exe); ok {
		t.Error("an agent the developer did not choose was checked")
	}
	if c, ok := AgentHooksCheck(ctx, reg, []string{"fake"}, ""); !ok || c.Status != Warn {
		t.Errorf("unknown terma: %+v, %v; want a warning, never silence", c, ok)
	}
	if _, ok := AgentHooksCheck(ctx, agents.New(fake), []string{"fake"}, exe); ok {
		t.Error("an agent this build does not offer was checked")
	}

	check(Fail, "fake 1.5.15")
	install(exe)
	check(Pass, "fake 1.5.15")

	if runtime.GOOS != "windows" {
		// Another path to this terma, as a second symlink or an install directory makes.
		link := filepath.Join(t.TempDir(), "terma")
		if err := os.Symlink(exe, link); err != nil {
			t.Fatal(err)
		}
		install(link)
		check(Pass, "fake")
	}

	other := binary(t, filepath.Join(t.TempDir(), "other", "terma"))
	install(other)
	check(Warn, "other")

	install(filepath.Join(t.TempDir(), "gone", "terma"))
	check(Fail, "out of date")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check(Fail, "hooks.json")
}
