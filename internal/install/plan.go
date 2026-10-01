package install

import (
	"fmt"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// RoutedSurfaces are the selected surfaces of the agents routed to the project.
func RoutedSurfaces(reg *agents.Registry, selected, targets []string) []string {
	var out []string
	for _, name := range selected {
		if _, a, ok := reg.Surface(name); ok && slices.Contains(targets, a.Name()) {
			out = append(out, name)
		}
	}
	return out
}

// SelectedSurfaces are the surfaces the selection names.
func SelectedSurfaces(reg *agents.Registry, selected []string) []agents.Surface {
	var out []agents.Surface
	for _, name := range selected {
		if s, _, ok := reg.Surface(name); ok {
			out = append(out, s)
		}
	}
	return out
}

// CheckSignalNeeds refuses signals a selected surface cannot report without.
func CheckSignalNeeds(reg *agents.Registry, selected []string, rawSignals string) error {
	for _, s := range SelectedSurfaces(reg, selected) {
		if len(s.Needs.Signals) == 0 {
			continue
		}
		signals, err := harness.ParseSignals(rawSignals)
		if err != nil {
			return err
		}
		for _, want := range s.Needs.Signals {
			if !slices.Contains(signals, harness.Signal(want)) {
				return fmt.Errorf("%s needs the %s signal to route sessions by repository", s.DisplayName, want)
			}
		}
	}
	return nil
}

// CheckHookNeeds refuses adapters without the committed hooks a selected surface needs.
func CheckHookNeeds(reg *agents.Registry, selected, adapters []string) error {
	for _, s := range SelectedSurfaces(reg, selected) {
		if _, a, _ := reg.Surface(s.Name); s.Needs.Hooks && !slices.Contains(adapters, a.Name()) {
			return fmt.Errorf("%s needs the %s repository hooks; include %s in --adapters", s.DisplayName, a.DisplayName(), a.Name())
		}
	}
	return nil
}

// CheckHooksApplied refuses an install that leaves out committed hooks a selected surface
// needs: its agent's plan must have nothing left to write.
func CheckHooksApplied(reg *agents.Registry, root string, selected []string, hint string) error {
	for _, s := range SelectedSurfaces(reg, selected) {
		if !s.Needs.Hooks {
			continue
		}
		_, a, _ := reg.Surface(s.Name)
		plan, err := a.Plan(root, true)
		if err != nil {
			return err
		}
		if !plan.Empty() {
			return fmt.Errorf("%s needs its %s repository hooks; %s", s.DisplayName, a.DisplayName(), hint)
		}
	}
	return nil
}

// Adapters lists the agents whose committed hooks to wire. --adapters overrides
// it outright; otherwise it is the union of the agents the repository's hooks files
// already wire, the agents this install configures, and any adapter whose directory the
// repository already carries (a .codex directory is a clear sign the repo is opened in
// Codex) — restricted to adapters that actually write a hooks file, and to agents that
// are available: one still coming soon (not supported by this build) is wired
// only when --adapters names it, whatever directory the repository carries. Hooks a
// colleague committed for one are left as they are, not rewritten or removed.
//
// A union (rather than replacing with the current selection) means selecting more agents
// grows the committed set, while a colleague re-running install with a narrower selection
// never removes hooks someone else committed — so the files grow on purpose and never
// churn down.
func Adapters(reg *agents.Registry, root string, selected, override []string) []string {
	if len(override) > 0 {
		return override
	}
	var out []string
	for _, a := range reg.All() {
		if a.HooksPath() == "" || !reg.IsSupported(a.Name()) {
			continue
		}
		chosen := slices.ContainsFunc(agents.Selections(a), func(s string) bool { return slices.Contains(selected, s) })
		if chosen || a.Default(root) || agents.Wired(root, a) {
			out = append(out, a.Name())
		}
	}
	return out
}

// Binding is the project a repository is tied to, and the environment it was chosen in.
type Binding struct {
	ID, Name, OrganizationID, Environment string
}

// Kept is the repository's Binding as it stands, environment included: a
// colleague confirming the project must not rewrite the committed file to match their
// own setup.
func Kept(existing *termaproject.File) Binding {
	p := existing.Project
	return Binding{ID: p.ID, Name: p.Name, OrganizationID: p.OrganizationID, Environment: p.Environment}
}

// PolicyHarnesses is the subset of an install's adapters whose harness reads a
// repository's own export policy. Cursor and Codex are wired for hooks and nothing
// else: Cursor has no local OTLP exporter policy, and Codex ignores an otel table
// in a project's config.
func PolicyHarnesses(reg *agents.Registry, adapters []string) []harness.Harness {
	var out []harness.Harness
	for _, a := range adapters {
		h, err := reg.Harness(a)
		if err != nil {
			continue
		}
		if _, ok := h.(harness.Scoped); ok {
			out = append(out, h)
		}
	}
	return out
}
