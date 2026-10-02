package install

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// ErrNotSignedIn is a binding that could not be resolved without a sign-in; a dry run
// plans without one.
var ErrNotSignedIn = errors.New("not signed in")

// Request is what the developer asked of an install.
type Request struct {
	Root, GitDir string
	// Existing is the workspace's binding, a linked worktree's main checkout's when it has
	// none (Open).
	Existing   *termaproject.File
	ProjectRef string
	Selected   []string
	// RecordSelected saves Selected as the developer's agents, once the install is admitted.
	RecordSelected    bool
	Adapters          []string
	NoHooks, DryRun   bool
	Prompts, Content  *bool
	AssumeYes, CanAsk bool
	Version           string
	Now               time.Time
}

// Workflow is what an install reaches through the command line. SignIn, Bind,
// FetchPolicy and ApplySteps are required; the rest do nothing when nil.
type Workflow struct {
	// SignIn signs the developer in and returns the configuration reloaded under it.
	SignIn func(ctx context.Context, cfg *config.Config) (*config.Config, error)
	// Bind resolves the project to bind to, checking an existing binding against the
	// credential's projects when verify. ErrNotSignedIn means it needs a sign-in.
	Bind func(ctx context.Context, cfg *config.Config, existing *termaproject.File, ref string, verify, ask bool) (Binding, error)
	// FetchPolicy is the organization's collection policy for cfg's project.
	FetchPolicy func(ctx context.Context, cfg *config.Config) (config.Policy, error)
	// HasKey reports whether this machine holds projectID's key.
	HasKey func(projectID string) bool
	// ApplySteps are the plan's steps outside the repository, under the final cfg.
	ApplySteps func(cfg *config.Config, p Plan) Steps
	// RefreshMachine refreshes the home-directory files an earlier terma wrote.
	RefreshMachine func() ([]string, error)
}

// Open checks the files an install writes and resolves the workspace's binding.
func Open(reg *agents.Registry, root, gitDir string) (*termaproject.File, error) {
	for _, path := range append([]string{termaproject.FileName}, reg.HooksPaths()...) {
		if err := termaproject.CheckPath(root, path); err != nil {
			return nil, err
		}
	}
	// A linked worktree keeps its main checkout's project; the binding it writes is its own.
	existing, _, err := termaproject.Resolve(root, gitDir)
	if err != nil && !errors.Is(err, termaproject.ErrNotFound) {
		return nil, err
	}
	return existing, nil
}

// Run installs terma into the workspace. In order: sign-in when the install needs it,
// the binding, the organization's policy and its admission, the plan, then — only once
// admitted — everything that writes. A dry run signs in to nothing, writes nothing, and
// prints the plan a real install would apply.
func Run(ctx context.Context, reg *agents.Registry, cfg *config.Config, req Request, w Workflow, r Reporter) (Plan, error) {
	if w.SignIn == nil || w.Bind == nil || w.FetchPolicy == nil || w.ApplySteps == nil {
		return Plan{}, errors.New("install: sign-in, binding, the policy fetch and the apply steps are required")
	}
	// Every real install reads the team's policy, even hooks-only; only an offline policy
	// fixture skips that login. A dry run never signs in.
	needsAuth := cfg.APIKey == "" && (config.PolicyStub() == "" || NeedsAuth(reg, req, w.HasKey))
	if needsAuth && !req.DryRun {
		var err error
		if cfg, err = w.SignIn(ctx, cfg); err != nil {
			return Plan{}, err
		}
	}
	b, err := w.Bind(ctx, cfg, req.Existing, req.ProjectRef, needsAuth, req.CanAsk && !req.AssumeYes && !req.DryRun)
	if err != nil {
		// A signed-out dry run plans against an unresolved project: its files do not
		// depend on the project id.
		if !req.DryRun || !errors.Is(err, ErrNotSignedIn) {
			return Plan{}, err
		}
		b = Binding{}
	}
	cfg.ProjectID, cfg.ProjectName, cfg.OrganizationID = b.ID, b.Name, b.OrganizationID
	var admitting *config.Policy
	if !req.DryRun {
		pol, err := w.FetchPolicy(ctx, cfg)
		if err != nil {
			return Plan{}, err
		}
		// The fetch is the command line's; what it returns must still be this login's.
		if !pol.AppliesTo(cfg.OrganizationID, cfg.AuthURL) {
			return Plan{}, errors.New("install: the collection policy fetched belongs to another organization or environment")
		}
		if err := Admit(pol, req.Existing, b); err != nil {
			return Plan{}, err
		}
		if err := routing.StorePolicy(cfg, &pol); err != nil {
			return Plan{}, err
		}
		cfg.Policy, admitting = pol, &pol
		if req.RecordSelected {
			if err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) { p.Harnesses = req.Selected }); err != nil {
				return Plan{}, err
			}
		}
	}
	// Built once, so a dry run prints exactly the plan an install applies.
	plan, err := Build(reg, Input{Root: req.Root, GitDir: req.GitDir, Existing: req.Existing, Selected: req.Selected,
		Adapters: req.Adapters, NoHooks: req.NoHooks, Binding: b, Prompts: req.Prompts, ToolContent: req.Content, Policy: admitting})
	if err != nil {
		return Plan{}, err
	}
	summarize(r, plan, b, cfg.Environment, req.GitDir)
	if req.DryRun {
		return plan, plan.PrintDryRun(r.Detail(), needsAuth)
	}
	if err := Apply(ctx, plan, Options{AssumeYes: req.AssumeYes, Version: req.Version, Now: req.Now}, w.ApplySteps(cfg, plan), r); err != nil {
		return plan, err
	}
	// A newer release's first install refreshes the machine before doctor checks it, and
	// records it so the refresh after the command has nothing left to do.
	if dir, err := config.Dir(); err == nil && w.RefreshMachine != nil && selfupdate.NeedsRefresh(dir, req.Version) {
		changed, err := w.RefreshMachine()
		for _, p := range changed {
			fmt.Fprintf(r.Detail(), "  updated %s\n", p)
		}
		if err != nil {
			r.Warn("Refreshed", "some files an earlier terma installed could not be updated ("+err.Error()+")")
			r.Then("Run `terma update` to retry.")
		} else {
			if len(changed) > 0 {
				r.OK("Refreshed", fmt.Sprintf("%d file(s) an earlier terma installed", len(changed)))
			}
			_ = selfupdate.SaveRefreshed(dir, req.Version)
		}
	}
	r.Then("Run `terma doctor` any time to check everything works.")
	return plan, nil
}

func summarize(r Reporter, plan Plan, b Binding, env, gitDir string) {
	if gitDir == "" {
		r.Warn("Git hooks", "skipped — not a Git repository, so commits are not stamped")
		r.Then("Run `terma install` inside a Git repository to stamp its commits.")
	}
	if env != config.EnvProd {
		env = " (" + env + ")"
	} else {
		env = ""
	}
	if b.ID == "" && b.Name == "" {
		r.Warn("Team", "unresolved — a real install signs in and selects one"+env)
	} else {
		r.Summary("Team", cmp.Or(b.Name, b.ID)+env)
	}
	// Nothing asks, so the line names the command that changes it.
	if len(plan.Agents.RelayTargets(plan.Selected)) > 0 {
		if plan.Prompts {
			r.Summary("Prompts", "sent — `terma install --prompts off` stops them")
		} else {
			r.Summary("Prompts", "not sent — `terma install --prompts on` sends them")
		}
	}
}

// NeedsAuth reports whether an install signs in: a telemetry agent's key, a project to
// look up, or the spool key a hooks-only agent gets from nowhere else.
func NeedsAuth(reg *agents.Registry, req Request, hasKey func(string) bool) bool {
	for _, a := range telemetryAgentNames(reg, req.Selected) {
		if _, err := reg.Harness(a); err == nil {
			return true
		}
	}
	ref := strings.TrimSpace(req.ProjectRef)
	if (ref == "" && req.Existing == nil) || (ref != "" && !termaproject.ValidID(ref)) {
		return true
	}
	projectID := ref
	if projectID == "" {
		projectID = req.Existing.Project.ID
	}
	return !req.NoHooks && len(req.Selected) > 0 && (hasKey == nil || !hasKey(projectID))
}

// telemetryAgentNames are the agents behind the selected surfaces, once each.
func telemetryAgentNames(reg *agents.Registry, selected []string) []string {
	var names []string
	for _, name := range selected {
		if _, a, ok := reg.Surface(name); ok {
			name = a.Name()
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}
