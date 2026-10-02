package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// DryRun is what a dry run reports beyond the plan: the sign-in a real install starts
// with, and the steps it would take, asked the same questions Apply asks.
type DryRun struct {
	SignIn bool
	// HasKey reports whether this machine holds a project's key.
	HasKey func(projectID string) bool
	// Steps are the ones Apply would be handed; a dry run runs none of them.
	Steps   Steps
	Options Options
	// RefreshMachine is a newer release's first install refreshing the home directory.
	RefreshMachine bool
}

// PrintDryRun says what an install would do: the files it writes, then every change it
// makes outside them, in Apply's order.
func (p Plan) PrintDryRun(ctx context.Context, w io.Writer, d DryRun) error {
	if !p.NoHooks {
		p.Hooks.Print(w)
	}
	if d.SignIn {
		fmt.Fprintln(w, "\nA real install would sign in first (not done for a dry run).")
	}
	for _, h := range PolicyHarnesses(p.Agents, p.Adapters, p.Root) {
		path, err := h.ConfigPath()
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "\nRepository telemetry: %s (preserve existing policy unless export flags are supplied).\n", path)
	}
	if also := p.changes(ctx, d); len(also) > 0 {
		fmt.Fprintln(w, "\nA real install would also:")
		for _, c := range also {
			fmt.Fprintf(w, "  - %s\n", c)
		}
	}
	for _, step := range p.stepsAfter() {
		fmt.Fprintf(w, "\nAfter a real install — %s\n", step)
	}
	fmt.Fprintln(w, "\nDry run: nothing written.")
	return nil
}

// changes are what Apply would change beyond the hook files and repository policy.
func (p Plan) changes(ctx context.Context, d DryRun) []string {
	reg := p.Agents
	var out []string
	if targets := reg.RelayTargets(p.Selected); d.Steps.Connect != nil && len(targets) > 0 {
		out = append(out, "send "+output.And(p.displayNames(targets))+" sessions to this team through the local relay (settings in your home directory)")
	}
	if d.Steps.StatusLine != nil {
		out = append(out, "wrap the status line, to read your plan's usage windows")
	}
	installedHooks := p.GitDir != "" && (!p.NoHooks || p.Existing != nil && p.Existing.Install.HookManager != "")
	for _, a := range p.pendingTrust() {
		out = append(out, "approve Terma's hooks in "+a.DisplayName()+", so it runs them with no review step")
	}
	wired := !p.NoHooks && len(p.Adapters) > 0 || len(reg.WiredNames(p.Root)) > 0
	if d.Steps.SpoolKey != nil && (installedHooks || wired) && (d.HasKey == nil || !d.HasKey(p.Binding.ID)) {
		out = append(out, "mint this machine a key for the team's hook events")
	}
	file := p.Binding.File(p.Existing, p.Detection, installedHooks, !p.Hooks.Empty(), d.Options)
	if !sameBinding(p.Root, file) {
		out = append(out, "write "+termaproject.FileName)
	}
	if installedHooks && file.Install.HookManager == string(hookmgr.GitShim) && !shimsWired(ctx, p.Root, p.GitDir) {
		out = append(out, "point git at the committed hook shims (core.hooksPath = "+hookmgr.ShimDir+")")
	}
	if d.RefreshMachine {
		out = append(out, "refresh the files an earlier terma installed in your home directory")
	}
	return out
}

// pendingTrust are the agents whose approvals Apply would write: wired once the plan is
// applied, and not already trusting the file as it will stand.
func (p Plan) pendingTrust() []agents.Agent {
	if p.NoHooks {
		return nil
	}
	planned := p.Hooks.Paths()
	var out []agents.Agent
	for _, a := range ownTrust(p.Agents, p.wiredAfter) {
		if t, ok := a.(agents.Trusting); ok && !slices.Contains(planned, a.HooksPath()) {
			if st, err := t.Trust(p.Root); err == nil && st.Trusted {
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// stepsAfter are the selected surfaces' steps Apply would print: none for an agent whose
// hooks it approves itself, or that already trusts them.
func (p Plan) stepsAfter() []string {
	approves := map[string]bool{}
	if !p.NoHooks {
		for _, a := range ownTrust(p.Agents, p.wiredAfter) {
			approves[a.Name()] = true
		}
	}
	var out []string
	for _, s := range SelectedSurfaces(p.Agents, p.Selected) {
		if _, a, _ := p.Agents.Surface(s.Name); a != nil {
			if approves[a.Name()] {
				continue
			}
			if t, ok := a.(agents.Trusting); ok {
				if st, err := t.Trust(p.Root); err == nil && st.Trusted {
					continue
				}
			}
		}
		out = append(out, s.InstallSteps...)
	}
	return out
}

// wiredAfter reports whether a's hooks are wired once the plan is applied.
func (p Plan) wiredAfter(a agents.Agent) bool {
	return slices.Contains(p.Adapters, a.Name()) || agents.Wired(p.Root, a)
}

func (p Plan) displayNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if a, ok := p.Agents.Lookup(n); ok {
			out = append(out, a.DisplayName())
		}
	}
	return out
}

// sameBinding reports whether the binding at root already reads as f, so saving it
// changes nothing.
func sameBinding(root string, f *termaproject.File) bool {
	have, err := termaproject.Load(root)
	if err != nil {
		return false
	}
	a, errA := json.Marshal(have)
	b, errB := json.Marshal(f)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}
