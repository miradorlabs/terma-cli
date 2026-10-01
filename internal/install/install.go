// Package install puts terma into a repository: the commit hooks, each agent's own
// hooks, and the committed binding. A Plan is built once, before anything is written; a
// dry run prints it and Apply carries it out, with what lies outside the repository —
// the agents' configuration, keys, the export policy — supplied as Steps.
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
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/output"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// Plan is what an install does in a repository.
type Plan struct {
	Agents       *agents.Registry
	Root, GitDir string
	// Existing is the repository's binding before the install, nil for a first one.
	Existing *termaproject.File
	// Selected are the developer's agents; Adapters the agents whose committed hooks
	// are wired.
	Selected, Adapters []string
	Detection          hookmgr.Detection
	// Hooks is what the hooks would write; NoHooks leaves them as they are.
	Hooks   HookPlan
	NoHooks bool
	Binding Binding
}

// Build plans an install of the selected agents into the workspace at root. adapters,
// when given, names the wired agents outright.
func Build(reg *agents.Registry, root, gitDir string, existing *termaproject.File, selected, adapters []string, noHooks bool, b Binding) (Plan, error) {
	p := Plan{Agents: reg, Root: root, GitDir: gitDir, Existing: existing, Selected: selected, NoHooks: noHooks, Binding: b}
	// The wired adapters are a team decision, so a re-install keeps every agent the
	// repository's hooks files already wire: a colleague re-running install must not
	// rewrite the committed hooks to match their own agent set.
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

// PrintDryRun says what an install would do. signIn says it would sign in first.
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
	// Detail takes the long form: the plan's file list, the git wiring.
	Detail() io.Writer
}

// Steps are what an install does outside the repository. A nil step is skipped.
type Steps struct {
	// Confirm asks before the hooks are written; an error stops the install.
	Confirm func(question string, explain []string) (bool, error)
	// Connect points the developer's agents at the repository's project.
	Connect func(ctx context.Context) error
	// StatusLine wraps the agent's status line, saying how.
	StatusLine func() (note string, ok bool)
	// SpoolKey makes sure this machine can deliver the repository's hook events: state
	// says how they go, and fix, when set, what the developer must do first.
	SpoolKey func(ctx context.Context) (state, fix string)
	// RepoPolicy writes the repository's export policy for the agents that read one,
	// returning the files it wrote.
	RepoPolicy func(ctx context.Context, hs []harness.Harness) ([]string, error)
}

// Options are an install's own choices.
type Options struct {
	// AssumeYes writes the hooks without asking.
	AssumeYes bool
	// Version is this terma's, stamped on the binding when the install writes a file.
	Version string
	Now     time.Time
}

// Apply carries out the plan.
func Apply(ctx context.Context, p Plan, o Options, s Steps, r Reporter) error {
	reg := p.Agents
	// Reserve the private store even before the first agent event. A hook already
	// in flight when git init runs and a hook starting afterwards must choose the
	// same store, including when neither has written a manifest yet.
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
	var written []string    // the repository files this run wrote hooks into, to commit
	var afterMerge []string // what each clone does once they are merged
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
			// An empty git-hook plan means the commit hooks are already wired, so this
			// repo is hook-installed; record them in the binding without rewriting.
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
			adapters = reg.WiredNames(p.Root) // declined: only what is already wired
			r.Warn("Hooks", "not written — commits are not stamped until they are")
			r.Then("Run `terma install` again and accept the hooks when you are ready.")
		}
	} else {
		adapters = reg.WiredNames(p.Root) // --no-hooks: only what is already wired
	}
	if err := CheckHooksApplied(reg, p.Root, p.Selected, "run `terma install` without --no-hooks and accept the hook plan"); err != nil {
		return err
	}

	// The key this machine delivers the repository's hook events with: without one,
	// every commit, tool call and observation waits in the spool.
	if s.SpoolKey != nil && (installedHooks || len(reg.WiredNames(p.Root)) > 0) {
		if state, fix := s.SpoolKey(ctx); fix == "" {
			r.OK("Hook events", state)
		} else {
			r.Warn("Hook events", state)
			r.Then(fix)
		}
	}

	// Repository telemetry also supports developers using a global repos-only
	// connection. Hooks alone do not enable that connection's exporters.
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
		// Save always rewrites the binding.
		r.Commit("Commit these files and open a PR — merging it onboards the repository:", append(written, termaproject.FileName))
	}
	for _, n := range afterMerge {
		r.Then("After merging: " + n)
	}
	return nil
}

// File is the committed binding an install writes. installed_at is the onboarder's and
// never moves; terma_version is the terma that last wrote the committed files, so it
// moves only when this install wrote one — a colleague's install that changes nothing
// does not churn the file.
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
