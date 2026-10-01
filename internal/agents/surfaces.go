package agents

import (
	"context"
)

// Surface is one way a developer runs an agent, chosen on its own at setup (a CLI, a
// desktop app).
type Surface struct {
	Name, DisplayName string
	Installed         func(context.Context) bool
	// InstallSteps and SetupSteps are what the developer does next for it to report.
	InstallSteps, SetupSteps []string
	// Reports says, after an install, how the surface's sessions reach Terma.
	Reports string
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
