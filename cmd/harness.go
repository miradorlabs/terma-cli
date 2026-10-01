package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/output"
)

// newHarnessListCommand reports terma's static support for each coding agent: what it
// can do with it, and where a capability is missing. This is the catalog, not a
// connection — `terma harness status` reads each harness's own config for that.
func (app *App) newHarnessListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "list [" + strings.Join(app.agents.SupportNames(), "|") + "]",
		Aliases: []string{"ls"},
		Short:   "Show which coding agents terma supports, and how fully",
		Long: `Lists every coding agent terma integrates with and how completely each one works.

Two capabilities are reported per agent:

  attribution  stamps commits with the session and tool that produced them
  telemetry    exports usage to Terma so ` + "`terma usage`" + ` and ` + "`terma session`" + ` can
               report spend and cost

An agent is "full" when both work completely, "partial" when either has gaps; the
notes column says which. An agent with no telemetry export of its own reports through
its hooks: the model, turns and tool calls, without token counts or cost.

This is what terma can do, not what is wired up in this repository or on this machine —
run ` + "`terma harness status`" + ` for that.`,
		Args: cobra.MaximumNArgs(1),
		RunE: app.runHarnessList,
	}
}

func (app *App) runHarnessList(cmd *cobra.Command, args []string) error {
	format, err := app.resolveFormat()
	if err != nil {
		return err
	}

	catalog := app.agents.SupportCatalog()
	if len(args) == 1 {
		a, ok := app.agents.LookupSupport(args[0])
		if !ok {
			return fmt.Errorf("unknown agent %q (want %s)", args[0], strings.Join(app.agents.SupportNames(), ", "))
		}
		catalog = []agents.AgentSupport{a}
	}

	rows := make([][]string, 0, len(catalog))
	for _, a := range catalog {
		rows = append(rows, []string{
			a.DisplayName,
			string(a.Attribution.Level),
			string(a.Telemetry.Level),
			string(a.Support),
			supportNotes(a),
		})
	}

	return output.Render(cmd.OutOrStdout(), format, output.Table{
		Headers: []string{"HARNESS", "ATTRIBUTION", "TELEMETRY", "SUPPORT", "NOTES"},
		Rows:    rows,
	}, harnessSupportReport{Harnesses: catalog})
}

type harnessSupportReport struct {
	Harnesses []agents.AgentSupport `json:"harnesses"`
}

// supportNotes joins the per-capability caveats for the NOTES column, telemetry first
// because a missing telemetry capability is the gap a reader most needs to see.
func supportNotes(a agents.AgentSupport) string {
	var notes []string
	if a.Telemetry.Note != "" {
		notes = append(notes, a.Telemetry.Note)
	}
	if a.Attribution.Note != "" {
		notes = append(notes, a.Attribution.Note)
	}
	return strings.Join(notes, "; ")
}
