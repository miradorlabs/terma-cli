package cli

import (
	"fmt"
	"io"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// setupStatusLine wraps the status line, a global setting, of each agent the developer chose
// that has one, and puts back the one terma wrapped for an agent no longer chosen, as its
// hooks are taken out.
func (app *App) setupStatusLine(errOut io.Writer, ui *setupUI, names []string) {
	lines := doctor.StatusLineAgents(app.agents)
	for _, a := range lines {
		if !slices.Contains(names, a.Name()) {
			if restored, err := a.RemoveStatusLine(); err != nil {
				fmt.Fprintf(errOut, "Warning: could not restore %s's status line: %v\n", a.DisplayName(), err)
			} else if restored {
				ui.OK(a.DisplayName()+" status line", "restored to what it was before terma")
			}
			continue
		}
		label := "Status line"
		if len(lines) > 1 {
			label = a.DisplayName() + " status line"
		}
		before, _ := a.StatusLineState("")
		if note, ok := wrapStatusLine(errOut, a); ok {
			fmt.Fprintf(ui.Detail(), "\n%s\n", note)
			ui.OK(label, "reads your plan's usage windows")
			if before.Disabled {
				ui.Caution(label, fmt.Sprintf(`switched back on in %s (it was "enabled": false)`, output.TildePath(before.ConfigPath)))
			}
		} else {
			ui.Warn(label, "not wrapped — your plan's usage windows are not captured")
			ui.Then("Run `terma doctor` to see why the status line was not wrapped.")
		}
	}
}

// wrapStatusLine only warns on failure: the exporters are already written and working.
func wrapStatusLine(errOut io.Writer, c agents.StatusLiner) (string, bool) {
	changed, err := c.InstallStatusLine()
	if err != nil {
		fmt.Fprintf(errOut, "Warning: could not wrap %s's status line (%v); plan usage will not be captured.\n", c.DisplayName(), err)
		return "", false
	}
	st, stErr := c.StatusLineState("")
	switch {
	case stErr != nil:
		return "", true
	case changed && st.Renderer != "":
		return fmt.Sprintf("Status line: terma now reads the plan's usage windows from it; your own status line (%s) keeps running unchanged behind it.", output.SanitizeTerminal(st.Renderer)), true
	case changed && st.Stacked:
		return fmt.Sprintf("Status line: terma now reads the plan's usage windows from it; %s's own status line stays as it was (`terma teardown` removes terma's).", c.DisplayName()), true
	case changed:
		return "Status line: terma added one that shows model, context, cost and the plan's usage windows (`terma teardown` removes it).", true
	default:
		return "Status line: already wrapped by terma.", true
	}
}
