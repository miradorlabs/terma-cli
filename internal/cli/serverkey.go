package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// agentKey is the agent's key for the configured project: the one it already exports
// with, else the one remembered for it, else a new one. Reusing one keeps tweaks from
// leaving live orphaned keys, and one agent's key can be revoked alone.
func (app *App) agentKey(ctx context.Context, cfg *config.Config, h harness.Harness) (string, error) {
	if existing, ok := h.CurrentCredential(cfg.OTLPURL, cfg.ProjectID); ok {
		return existing, nil
	}
	if remembered := keystore.GetFor(h.Name(), cfg.ProjectID); remembered != "" {
		return remembered, nil
	}
	client, err := app.newClient(cfg)
	if err != nil {
		return "", err
	}
	key, _, err := client.CreateServerKey(ctx, cfg.ProjectID, h.Name()+"@"+hostname(), "Created by terma install "+h.Name())
	return key, err
}

// installStatusLine only warns on failure: the exporters are already written and working.
func (app *App) installStatusLine(errOut io.Writer) (string, bool) {
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
		return "Status line: terma added one that shows model, context, cost and the plan's usage windows (`terma teardown` removes it; --no-statusline skips it).", true
	default:
		return "Status line: already wrapped by terma.", true
	}
}
