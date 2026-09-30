package cmd

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// The agents help text and messages name, read from the registry so a new agent is named
// wherever it applies.

// scopedHarnessNames lists the agents with repository settings of their own.
func scopedHarnessNames() string {
	var names []string
	for _, e := range registered.With[agents.Exporting]() {
		if _, ok := e.Harness().(harness.Scoped); ok {
			names = append(names, e.DisplayName())
		}
	}
	return strings.Join(names, ", ")
}

// statusLineOwner is the agent whose status line terma wraps.
func statusLineOwner() string {
	if a, ok := statusLineAgent(); ok {
		return a.DisplayName()
	}
	return "the agent"
}

// sourceExamples are the source systems the supported agents report under.
func sourceExamples() string {
	var labels []string
	for _, a := range registered.Supported() {
		labels = append(labels, agents.Tool(a))
	}
	return strings.Join(labels, ", ")
}

// supportedAgentNames lists the supported agents for prose.
func supportedAgentNames() string {
	var names []string
	for _, a := range registered.Supported() {
		names = append(names, a.DisplayName())
	}
	return strings.Join(names, ", ")
}
