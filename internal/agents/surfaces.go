package agents

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Surface is one way a developer runs an agent, chosen on its own at setup (a CLI, a
// desktop app).
type Surface struct {
	Name, DisplayName string
	Installed         func(context.Context) bool
	// Needs is what an install must give the surface for it to report at all.
	Needs Needs
	// InstallSteps and SetupSteps are what the developer does next for it to report.
	InstallSteps, SetupSteps []string
	// Reports says, after an install, how the surface's sessions reach Terma.
	Reports string
	// Warn is a machine condition worth knowing, continuing a sentence that starts with
	// the agent's name; "" when there is none.
	Warn func() string
}

// Needs is what a surface cannot report without.
type Needs struct {
	Signals []string
	Hooks   bool
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
	// Ready means the surface's sessions here reach Terma; Problem says why not, Fix what to run.
	Ready        bool
	Problem, Fix string
	// Lines are what `terma agent status` prints.
	Lines []StatusLine
}

// StatusLine is one labelled line of a surface's status.
type StatusLine struct{ Label, Value string }

// SurfaceChecker is an agent whose surfaces check their own readiness in a repository.
type SurfaceChecker interface {
	Agent
	CheckedSurfaces() []string
	CheckSurface(surface string, in SurfaceInput) (SurfaceStatus, error)
}

// SurfaceInput is what a surface's check reads, gathered by the caller.
type SurfaceInput struct {
	Root, ProjectID string
	Route           hookrun.Route
	Recorded        bool
	// RouteErr is a routing record that exists and cannot be read.
	RouteErr error
	// Keyed reports whether the agent, or else the project, has a delivery key.
	Keyed func(agent string) bool
}
