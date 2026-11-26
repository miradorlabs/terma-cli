package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// agentChoice is an onboarding surface: one repository adapter may be offered as several
// launch surfaces a developer picks independently.
type agentChoice struct {
	name      string
	display   string
	installed func(context.Context) bool
}

func (a agentChoice) Name() string        { return a.name }
func (a agentChoice) DisplayName() string { return a.display }
func (a agentChoice) Installed(ctx context.Context) bool {
	return a.installed(ctx)
}

// chooseHarnesses resolves the machine-level agent list: --harness, else a picker with the
// agents detected on this machine ticked, else (with --yes or no terminal) those agents.
func (app *App) chooseHarnesses(cmd *cobra.Command, f setupFlags) ([]string, error) {
	if strings.TrimSpace(f.harnesses) != "" {
		return app.parseAgentList(f.harnesses)
	}
	detected := app.detectedAgents(cmd.Context())
	canAsk := app.canAsk
	if canAsk == nil {
		canAsk = canPrompt
	}
	if f.assumeYes || !canAsk() {
		return app.selectedInRegistryOrder(detected), nil
	}

	fmt.Fprintln(cmd.OutOrStdout())
	form := app.harnessSelectionForm(detected)
	run := app.runForm
	if run == nil {
		run = prompt.Run
	}
	result, err := run(form)
	if errors.Is(err, prompt.ErrCancelled) {
		return nil, errCancelled
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for i, a := range app.harnessSelectionAgents() {
		if app.agents.IsSupported(a.Name()) && result[i].Selected {
			names = append(names, a.Name())
		}
	}
	return names, nil
}

func (app *App) availableAgentNames() []string {
	var names []string
	for _, a := range app.harnessSelectionAgents() {
		if app.agents.IsSupported(a.Name()) {
			names = append(names, a.Name())
		}
	}
	return names
}

// harnessSelectionAgents puts available agents first in registry order; the form and its
// result mapping must share this order.
func (app *App) harnessSelectionAgents() []agentChoice {
	var choices []agentChoice
	for _, available := range []bool{true, false} {
		for _, a := range app.agents.All() {
			if app.agents.IsSupported(a.Name()) == available {
				surfaces := agents.Surfaces(a)
				if !available { // Coming Soon: one row under its own name, no surface's beta mark
					surfaces = []agents.Surface{{Name: a.Name(), DisplayName: a.DisplayName(), Installed: a.Installed}}
				}
				for _, surface := range surfaces {
					choices = append(choices, agentChoice{name: surface.Name, display: surface.DisplayName, installed: surface.Installed})
				}
			}
		}
	}
	return choices
}

// harnessSelectionForm ticks and marks "detected" exactly the agents in detected.
func (app *App) harnessSelectionForm(detected map[string]bool) *prompt.Form {
	form := &prompt.Form{Title: "Which coding agents do you use?"}
	for _, a := range app.harnessSelectionAgents() {
		item := prompt.Item{Label: a.DisplayName(), Kind: prompt.Check}
		if app.agents.IsSupported(a.Name()) {
			item.Selected = detected[a.Name()]
			if item.Selected {
				item.Detail = "detected"
			}
		} else {
			item.Disabled = true
			item.Reason = "Coming Soon"
		}
		form.Items = append(form.Items, item)
	}
	for _, name := range app.agents.UpcomingNames() {
		form.Items = append(form.Items, prompt.Item{Label: name, Kind: prompt.Check, Disabled: true, Reason: "Coming Soon"})
	}
	return form
}

func (app *App) parseAgentList(raw string) ([]string, error) {
	seen := map[string]bool{}
	for _, n := range splitCommas(raw) {
		if _, _, ok := app.agents.Surface(n); !ok {
			return nil, fmt.Errorf("unknown agent %q (want %s)", n, joinNames(app.availableAgentNames()))
		}
		if !app.agents.IsSupported(n) {
			return nil, fmt.Errorf("agent %q: Coming Soon; available agents: %s", n, joinNames(app.availableAgentNames()))
		}
		seen[n] = true
	}
	return app.selectedInRegistryOrder(seen), nil
}

func (app *App) selectedInRegistryOrder(chosen map[string]bool) []string {
	var names []string
	for _, a := range app.harnessSelectionAgents() {
		if app.agents.IsSupported(a.Name()) && chosen[a.Name()] {
			names = append(names, a.Name())
		}
	}
	return names
}

// detectedAgents is the supported agents installed on this machine, each probed once.
func (app *App) detectedAgents(ctx context.Context) map[string]bool {
	detected := map[string]bool{}
	for _, a := range app.harnessSelectionAgents() {
		if app.agents.IsSupported(a.Name()) && a.Installed(ctx) {
			detected[a.Name()] = true
		}
	}
	return detected
}

func (app *App) adapterDisplayNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if s, _, ok := app.agents.Surface(n); ok {
			out = append(out, s.DisplayName)
		} else {
			out = append(out, n)
		}
	}
	return out
}
