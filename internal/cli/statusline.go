package cli

import (
	"fmt"
	"io"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// setupStatusLine wraps the status line, a global setting, for a developer who chose its agent.
func (app *App) setupStatusLine(errOut io.Writer, ui *setupUI, names []string) {
	a, ok := doctor.StatusLineAgent(app.agents)
	if !ok || !slices.Contains(names, a.Name()) {
		return
	}
	if note, ok := app.wrapStatusLine(errOut); ok {
		fmt.Fprintf(ui.Detail(), "\n%s\n", note)
		ui.OK("Status line", "reads your plan's usage windows")
	} else {
		ui.Warn("Status line", "not wrapped — your plan's usage windows are not captured")
		ui.Then("Run `terma doctor` to see why the status line was not wrapped.")
	}
}

// wrapStatusLine only warns on failure: the exporters are already written and working.
func (app *App) wrapStatusLine(errOut io.Writer) (string, bool) {
	c, ok := doctor.StatusLineAgent(app.agents)
	if !ok {
		return "", false
	}
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
	case changed:
		return "Status line: terma added one that shows model, context, cost and the plan's usage windows (`terma teardown` removes it).", true
	default:
		return "Status line: already wrapped by terma.", true
	}
}
