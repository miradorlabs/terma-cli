// Package install puts terma into a repository: the commit hooks, each agent's hooks and
// the committed binding. A Plan is built before anything is written, and what lies
// outside the repository is supplied as Steps.
package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// Plan is what an install does in a repository.
type Plan struct {
	Agents       *agents.Registry
	Root, GitDir string
	// Existing is nil for a first install.
	Existing *termaproject.File
	// Selected are the developer's agents; Adapters those whose committed hooks are wired.
	Selected, Adapters []string
	Detection          hookmgr.Detection
	Hooks              HookPlan
	NoHooks            bool
	Binding            Binding
}

// Build plans an install of the selected agents into the workspace at root.
func Build(reg *agents.Registry, root, gitDir string, existing *termaproject.File, selected, adapters []string, noHooks bool, b Binding) (Plan, error) {
	p := Plan{Agents: reg, Root: root, GitDir: gitDir, Existing: existing, Selected: selected, NoHooks: noHooks, Binding: b}
	// The wired adapters are a team decision: a colleague's re-install keeps them all.
	p.Adapters = Adapters(reg, root, selected, adapters)
	if err := CheckHookNeeds(reg, selected, p.Adapters); err != nil {
		return Plan{}, err
	}
	if gitDir != "" {
		p.Detection = hookmgr.Detect(root)
	}
	if noHooks {
		return p, CheckHooksApplied(reg, root, selected, "run `terma install` without --no-hooks")
	}
	var err error
	p.Hooks, err = PlanHooks(reg, root, p.Detection, p.Adapters)
	return p, err
}

// PrintDryRun says what an install would do.
func (p Plan) PrintDryRun(w io.Writer, signIn bool) error {
	if !p.NoHooks {
		p.Hooks.Print(w)
	}
	if signIn {
		fmt.Fprintln(w, "\nA real install would sign in first (not done for a dry run).")
	}
	for _, h := range PolicyHarnesses(p.Agents, p.Adapters) {
		path, err := h.(harness.Scoped).Local(p.Root).ConfigPath()
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "\nRepository telemetry: %s (preserve existing policy unless export flags are supplied).\n", path)
	}
	for _, s := range SelectedSurfaces(p.Agents, p.Selected) {
		for _, step := range s.SetupSteps {
			fmt.Fprintf(w, "\nAfter a real install — %s\n", step)
		}
	}
	fmt.Fprintln(w, "\nDry run: nothing written.")
	return nil
}

// Reporter is how an install reports, a line per step.
type Reporter interface {
	// OK reports a step done; Warn one that needs the developer.
	OK(label, what string)
	Warn(label, what string)
	// Then is a next step for the developer, and Commit the files they commit.
	Then(step string)
	Commit(lead string, paths []string)
	// Detail takes the long form.
	Detail() io.Writer
}

// Steps are what an install does outside the repository. A nil step is skipped.
type Steps struct {
	Confirm    func(question string, explain []string) (bool, error)
	Connect    func(ctx context.Context) error
	StatusLine func() (note string, ok bool)
	// SpoolKey makes sure this machine can deliver hook events; fix is what the
	// developer must do first.
	SpoolKey func(ctx context.Context) (state, fix string)
	// RepoPolicy writes the repository's export policy, returning the files it wrote.
	RepoPolicy func(ctx context.Context, hs []harness.Harness) ([]string, error)
}

// Options are an install's own choices.
type Options struct {
	AssumeYes bool
	Version   string
	Now       time.Time
}

// Apply carries out the plan.
func Apply(ctx context.Context, p Plan, o Options, s Steps, r Reporter) error {
	reg := p.Agents
	// Reserve the private store now, so hooks before and after a later git init choose the same one.
	if p.GitDir == "" {
		stateDir, err := termaproject.StateDir(p.Root, "")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return err
		}
	}
	if s.Connect != nil {
		if err := s.Connect(ctx); err != nil {
			return err
		}
	}
	if s.StatusLine != nil {
		if note, ok := s.StatusLine(); ok {
			fmt.Fprintf(r.Detail(), "\n%s\n", note)
			r.OK("Status line", "reads your plan's usage windows")
		} else {
			r.Warn("Status line", "not wrapped — your plan's usage windows are not captured")
		}
	}

	installedHooks := p.GitDir != "" && p.Existing != nil && p.Existing.Install.HookManager != ""
	adapters := p.Adapters
	var written []string
	var afterMerge []string
	if !p.NoHooks {
		p.Hooks.Print(r.Detail())
		write := o.AssumeYes
		if !p.Hooks.Empty() && !write && s.Confirm != nil {
			var err error
			if write, err = s.Confirm("Write terma's hooks to "+output.And(p.Hooks.Files())+"?", p.Hooks.Explain()); err != nil {
				return err
			}
		}
		switch {
		case p.Hooks.Empty():
			installedHooks = p.GitDir != ""
			r.OK("Hooks", p.Hooks.Summary(adapters)+" — already in place")
		case write:
			if err := p.Hooks.Apply(p.Root); err != nil {
				return err
			}
			installedHooks = p.GitDir != ""
			written = p.Hooks.Paths()
			r.OK("Hooks", p.Hooks.Summary(adapters))
			afterMerge = p.Hooks.hooks.Notes
		default:
			adapters = reg.WiredNames(p.Root)
			r.Warn("Hooks", "not written — commits are not stamped until they are")
			r.Then("Run `terma install` again and accept the hooks when you are ready.")
		}
	} else {
		adapters = reg.WiredNames(p.Root)
	}
	if err := CheckHooksApplied(reg, p.Root, p.Selected, "run `terma install` without --no-hooks and accept the hook plan"); err != nil {
		return err
	}

	// Without a spool key every hook event waits in the spool.
	if s.SpoolKey != nil && (installedHooks || len(reg.WiredNames(p.Root)) > 0) {
		if state, fix := s.SpoolKey(ctx); fix == "" {
			r.OK("Hook events", state)
		} else {
			r.Warn("Hook events", state)
			r.Then(fix)
		}
	}

	if s.RepoPolicy != nil {
		paths, err := s.RepoPolicy(ctx, PolicyHarnesses(reg, adapters))
		if err != nil {
			return err
		}
		for _, path := range paths {
			if !slices.Contains(written, path) {
				written = append(written, path)
			}
		}
	}

	file := p.Binding.File(p.Existing, p.Detection, installedHooks, len(written) > 0, o)
	if err := termaproject.Save(p.Root, file); err != nil {
		return err
	}
	if installedHooks {
		if err := Wire(ctx, r.Detail(), p.Root, file); err != nil {
			return err
		}
	}
	for _, s := range SelectedSurfaces(reg, p.Selected) {
		for _, step := range s.InstallSteps {
			r.Then(step)
		}
		if s.Warn == nil {
			continue
		}
		if warning := s.Warn(); warning != "" {
			_, a, _ := reg.Surface(s.Name)
			r.Warn(a.DisplayName(), warning)
		}
	}
	if p.GitDir != "" && len(written) > 0 {
		r.Commit("Commit these files and open a PR — merging it onboards the repository:", append(written, termaproject.FileName))
	}
	for _, n := range afterMerge {
		r.Then("After merging: " + n)
	}
	return nil
}

// File is the committed binding an install writes; terma_version moves only when this
// install wrote a file, so a no-op re-install does not churn it.
func (b Binding) File(existing *termaproject.File, det hookmgr.Detection, installedHooks, wrote bool, o Options) *termaproject.File {
	version, installedAt := o.Version, o.Now.UTC()
	if existing != nil {
		if existing.Install.Version != "" && !wrote {
			version = existing.Install.Version
		}
		if !existing.Install.InstalledAt.IsZero() {
			installedAt = existing.Install.InstalledAt
		}
	}
	return &termaproject.File{
		Project: termaproject.Project{ID: b.ID, Name: b.Name, OrganizationID: b.OrganizationID, Environment: b.Environment},
		Install: termaproject.Install{
			HookManager: managerOrEmpty(det, installedHooks),
			Hooks:       hooksOrNil(installedHooks),
			Version:     version,
			InstalledAt: installedAt,
		},
	}
}
