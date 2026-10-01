package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/install"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

func (app *App) newUninstallCommand() *cobra.Command {
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove everything terma install wrote to this repository",
		Long: `Symmetric with install: removes terma's hook wiring (only terma's lines from
shared hook files), each agent's committed hooks, .terma/settings.json, the per-clone
git configuration, and the local session state. Removing the binding un-routes this
checkout; the home-dir routing state (keys, routing records) is kept, since it is shared
with any other worktree or clone bound to the same project — remove it machine-wide
with 'terma nate'.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			root, gitDir, err := workspaceHere(ctx)
			if err != nil {
				return err
			}
			rm, err := install.PlanRemoval(ctx, app.agents, root, gitDir)
			if err != nil {
				return err
			}
			for _, note := range rm.Hooks.Notes {
				fmt.Fprintln(out, note)
			}
			if rm.Empty() {
				fmt.Fprintln(out, "Nothing of terma's is installed here.")
				return nil
			}
			fmt.Fprintln(out, "Files:")
			for _, c := range rm.Changes() {
				fmt.Fprintf(out, "  %-7s %s\n", c.Action(), c.Path)
			}
			if rm.Existing != nil {
				fmt.Fprintf(out, "  %-7s %s\n", "delete", termaproject.FileName)
			}
			if rm.RestoresHooksPath {
				fmt.Fprintln(out, "  restore git config core.hooksPath")
			}
			if !assumeYes {
				ok, err := confirm(cmd, "Remove these?")
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(out, "Cancelled. Nothing was removed.")
					return nil
				}
			}
			if err := rm.Apply(ctx, app.agents, func(warning string) { fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", warning) }); err != nil {
				return err
			}
			if gitDir != "" {
				fmt.Fprintln(out, "Uninstalled. Commit the removals if the install was committed.")
			} else {
				fmt.Fprintln(out, "Uninstalled.")
			}
			fmt.Fprintln(out, "This machine's routing records and keys stay — run `terma nate` when you no longer route any repository.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}
