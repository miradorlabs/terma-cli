package cli

import (
	"github.com/miradorlabs/terma-cli/internal/doctor"
)

// Agent names in help text come from the registry, so a new agent is named wherever it applies.

func (app *App) statusLineOwner() string {
	if a, ok := doctor.StatusLineAgent(app.agents); ok {
		return a.DisplayName()
	}
	return "the agent"
}
