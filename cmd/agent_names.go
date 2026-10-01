package cmd

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// The agents help text and messages name, read from the registry so a new agent is named
// wherever it applies.

// scopedHarnessNames lists the agents with repository settings of their own.
func (app *App) scopedHarnessNames() string {
	var names []string
	for _, e := range app.agents.With[agents.Exporting]() {
		if _, ok := e.Harness().(harness.Scoped); ok {
			names = append(names, e.DisplayName())
		}
	}
	return strings.Join(names, ", ")
}

// statusLineOwner is the agent whose status line terma wraps.
func (app *App) statusLineOwner() string {
	if a, ok := doctor.StatusLineAgent(app.agents); ok {
		return a.DisplayName()
	}
	return "the agent"
}

// sourceExamples are the source systems the supported agents report under.
func (app *App) sourceExamples() string {
	var labels []string
	for _, a := range app.agents.Supported() {
		labels = append(labels, agents.Tool(a))
	}
	return strings.Join(labels, ", ")
}
