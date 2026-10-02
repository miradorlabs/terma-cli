package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

type report struct {
	ok, warn, then []string
	commit         []string
}

func (r *report) OK(label, _ string)              { r.ok = append(r.ok, label) }
func (r *report) Warn(label, _ string)            { r.warn = append(r.warn, label) }
func (r *report) Then(step string)                { r.then = append(r.then, step) }
func (r *report) Commit(_ string, paths []string) { r.commit = paths }
func (r *report) Detail() io.Writer               { return io.Discard }
func (r *report) Summary(label, _ string)         { r.ok = append(r.ok, label) }

func plan(t *testing.T, root string, existing *termaproject.File) Plan {
	t.Helper()
	p, err := Build(builtin.Agents(), Input{Root: root, GitDir: root + "/.git", Existing: existing, Adapters: []string{"claude"},
		Binding: Binding{ID: "proj_1", Name: "One"}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Accepted hooks are written, the binding records them, git points at the shims, and
// every file written is named for the commit, the binding last.
func TestApplyWritesTheHooksAndTheBinding(t *testing.T) {
	root := hookruntest.InitRepo(t)
	r := &report{}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := Apply(context.Background(), plan(t, root, nil), Options{AssumeYes: true, Version: "v1.2.3", Now: now}, Steps{}, r); err != nil {
		t.Fatal(err)
	}
	f, err := termaproject.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if f.Project.ID != "proj_1" || f.Install.HookManager != string(hookmgr.GitShim) || f.Install.Version != "v1.2.3" || !f.Install.InstalledAt.Equal(now) {
		t.Fatalf("binding = %+v", f)
	}
	if got := gitx.ConfigGet(context.Background(), root, "core.hooksPath"); got != hookmgr.ShimDir {
		t.Fatalf("core.hooksPath = %q", got)
	}
	if !slices.Contains(r.commit, ".claude/settings.json") || r.commit[len(r.commit)-1] != termaproject.FileName {
		t.Fatalf("commit list = %v", r.commit)
	}
}

// Declined hooks are not written: the binding records none, the step is a warning, and
// the next step says how to accept them.
func TestApplyLeavesDeclinedHooksUnwritten(t *testing.T) {
	root := hookruntest.InitRepo(t)
	r := &report{}
	steps := Steps{Confirm: func(string, []string) (bool, error) { return false, nil }}
	if err := Apply(context.Background(), plan(t, root, nil), Options{Version: "v1"}, steps, r); err != nil {
		t.Fatal(err)
	}
	f, err := termaproject.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if f.Install.HookManager != "" || len(f.Install.Hooks) != 0 {
		t.Fatalf("declined hooks were recorded: %+v", f.Install)
	}
	if !slices.Contains(r.warn, "Hooks") || len(r.then) == 0 || !strings.Contains(r.then[0], "`terma install`") || r.commit != nil {
		t.Fatalf("report = %+v", r)
	}
}

// An install that writes nothing keeps the binding's version and install time.
func TestApplyThatWritesNothingKeepsTheBinding(t *testing.T) {
	root := hookruntest.InitRepo(t)
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := Apply(context.Background(), plan(t, root, nil), Options{AssumeYes: true, Version: "v1", Now: first}, Steps{}, &report{}); err != nil {
		t.Fatal(err)
	}
	existing, err := termaproject.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	r := &report{}
	if err := Apply(context.Background(), plan(t, root, existing), Options{AssumeYes: true, Version: "v2", Now: first.Add(time.Hour)}, Steps{}, r); err != nil {
		t.Fatal(err)
	}
	again, err := termaproject.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if again.Install.Version != "v1" || !again.Install.InstalledAt.Equal(first) || r.commit != nil {
		t.Fatalf("an install that wrote nothing churned the binding: %+v, commit %v", again.Install, r.commit)
	}
}

// The project's routing record is read into the plan, and a record that cannot be read
// stops the plan rather than being rewritten from defaults.
func TestBuildReadsTheRoutingRecord(t *testing.T) {
	root := hookruntest.InitRepo(t)
	in := Input{Root: root, GitDir: root + "/.git", Adapters: []string{"claude"}, Binding: Binding{ID: "proj_1"}}
	if p, err := Build(builtin.Agents(), in); err != nil || p.Record != nil {
		t.Fatalf("no record: %+v, %v", p, err)
	}
	if err := routing.SaveRecord(routing.Record{ProjectID: "proj_1", Signals: []string{"logs"}, Harnesses: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	if p, err := Build(builtin.Agents(), in); err != nil || p.Record == nil || len(p.Record.Signals) != 1 {
		t.Fatalf("the record was not read: %+v, %v", p, err)
	}
	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "routing", "proj_1.json"), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(builtin.Agents(), in); err == nil {
		t.Fatal("an unreadable routing record was planned over")
	}
}
