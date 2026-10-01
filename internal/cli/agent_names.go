package cli

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/doctor"
)

// Agent names in help text come from the registry, so a new agent is named wherever it applies.

func (app *App) scopedHarnessNames() string {
	var names []string
	for _, e := range app.agents.With[agents.Exporting]() {
		if _, ok := e.Harness().Local(""); ok {
			names = append(names, e.DisplayName())
		}
	}
	return strings.Join(names, ", ")
}

func (app *App) statusLineOwner() string {
	if a, ok := doctor.StatusLineAgent(app.agents); ok {
		return a.DisplayName()
	}
	return "the agent"
}
