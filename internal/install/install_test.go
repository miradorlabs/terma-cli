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
		Binding: Binding{ID: "proj_1", Name: "One"}, Policy: &config.Policy{Mode: config.ModeRepo, MembersCanAddRepositories: true}})
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

// A plan built without a policy, as a dry run is, cannot be applied.
func TestApplyNeedsAnAdmittedPlan(t *testing.T) {
	root := hookruntest.InitRepo(t)
	p, err := Build(builtin.Agents(), Input{Root: root, GitDir: root + "/.git", Adapters: []string{"claude"}, Binding: Binding{ID: "proj_1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), p, Options{AssumeYes: true}, Steps{}, &report{}); err == nil {
		t.Fatal("an unadmitted plan was applied")
	}
	if _, err := termaproject.Load(root); err == nil {
		t.Fatal("the refused plan wrote a binding")
	}
}

// A new binding is refused where the organization keeps adding repositories for its
// admins; a repository already bound to the project is not new.
func TestAdmitKeepsNewRepositoriesForAdmins(t *testing.T) {
	closed := config.Policy{Mode: config.ModeRepo}
	b := Binding{ID: "proj_1"}
	if err := Admit(closed, nil, b); err == nil {
		t.Fatal("a new repository was admitted")
	}
	if err := Admit(closed, &termaproject.File{Project: termaproject.Project{ID: "proj_1"}}, b); err != nil {
		t.Fatalf("a bound repository was refused: %v", err)
	}
	if err := Admit(config.Policy{Mode: config.ModeGlobal}, nil, b); err != nil {
		t.Fatalf("global mode refused: %v", err)
	}
}

// The project's last choice stands unless the developer makes one, and a record that
// cannot be read stops the plan rather than being rewritten from defaults.
func TestBuildResolvesContentFromTheRoutingRecord(t *testing.T) {
	root := hookruntest.InitRepo(t)
	in := Input{Root: root, GitDir: root + "/.git", Adapters: []string{"claude"}, Binding: Binding{ID: "proj_1"}}
	if p, err := Build(builtin.Agents(), in); err != nil || !p.Prompts || !p.ToolContent || p.Record != nil {
		t.Fatalf("no record: %+v, %v", p, err)
	}
	if err := routing.SaveRecord(routing.Record{ProjectID: "proj_1", IncludePrompts: false, IncludeToolContent: true, Harnesses: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	if p, err := Build(builtin.Agents(), in); err != nil || p.Prompts || !p.ToolContent || p.Record == nil {
		t.Fatalf("the record's prompts-off did not stand: %+v, %v", p, err)
	}
	in.Prompts = new(true)
	if p, err := Build(builtin.Agents(), in); err != nil || !p.Prompts {
		t.Fatalf("an explicit choice did not win: %+v, %v", p, err)
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
