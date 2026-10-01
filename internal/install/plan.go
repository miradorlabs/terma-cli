package install

import (
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

// Adapters lists the agents whose committed hooks to wire: override when given, else the
// union of the selected, the already wired and those whose directory the repository
// carries, among supported agents. A union, so a colleague's narrower re-install never
// removes hooks someone else committed.
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

// Kept is the repository's Binding as it stands, so a colleague's confirmation does not
// rewrite the committed file.
func Kept(existing *termaproject.File) Binding {
	p := existing.Project
	return Binding{ID: p.ID, Name: p.Name, OrganizationID: p.OrganizationID, Environment: p.Environment}
}

// PolicyHarnesses are the exporters, bound to the repository at root, of an install's
// adapters that read a repository's own export policy.
func PolicyHarnesses(reg *agents.Registry, adapters []string, root string) []harness.Harness {
	var out []harness.Harness
	for _, a := range adapters {
		h, err := reg.Harness(a)
		if err != nil {
			continue
		}
		if local, ok := h.Local(root); ok {
			out = append(out, local)
		}
	}
	return out
}
