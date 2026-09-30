package agents

import "context"

// Surface is one way a developer runs an agent, chosen on its own at setup: Codex's CLI
// and its desktop app are two.
type Surface struct {
	Name, DisplayName string
	Installed         func(context.Context) bool
	// Needs is what an install must give the surface for it to report at all.
	Needs Needs
	// InstallSteps and SetupSteps are what the developer does next for it to report.
	InstallSteps, SetupSteps []string
	// Reports says, after an install, how the surface's sessions reach Terma.
	Reports string
	// Warn is a condition on this machine the developer should know about, continuing a
	// sentence that starts with the agent's name; "" when there is none.
	Warn func() string
}

// Needs is what a surface cannot report without.
type Needs struct {
	Signals []string
	// Hooks: the agent's committed hooks, wired and applied.
	Hooks bool
}

// Surfaced is an agent run as more than one surface.
type Surfaced interface {
	Surfaces() []Surface
}

// Surfaces is a's surfaces: its own, or the agent itself as its one.
func Surfaces(a Agent) []Surface {
	if s, ok := a.(Surfaced); ok {
		return s.Surfaces()
	}
	return []Surface{{Name: a.Name(), DisplayName: a.DisplayName(), Installed: a.Installed}}
}

// SurfaceStatus is what a surface's own check found in a repository.
type SurfaceStatus struct {
	// Ready: the surface's sessions here reach Terma.
	Ready bool
	// Problem says why they do not, and Fix what to run.
	Problem, Fix string
	// Lines are what `terma agent status` prints, in order.
	Lines []StatusLine
}

// StatusLine is one labelled line of a surface's status.
type StatusLine struct{ Label, Value string }

// SurfaceChecker is an agent whose surfaces check their own readiness in a repository.
type SurfaceChecker interface {
	Agent
	// CheckedSurfaces names the surfaces with a check.
	CheckedSurfaces() []string
	// CheckSurface is one of them's status in the repository at root, bound to projectID.
	CheckSurface(surface, root, projectID string) (SurfaceStatus, error)
}
