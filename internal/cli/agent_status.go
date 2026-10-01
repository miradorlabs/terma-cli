package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

func (app *App) newAgentCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Inspect how an agent's surface reports from this repository", Hidden: true}
	cmd.AddCommand(&cobra.Command{
		Use:   "status <surface>",
		Short: "Show whether a surface's sessions in this repository reach Terma",
		Args:  cobra.ExactArgs(1),
		RunE:  app.statusAgentSurface,
	})
	return cmd
}

func (app *App) statusAgentSurface(cmd *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	binding, root, err := termaproject.ResolveDir(cwd)
	if errors.Is(err, termaproject.ErrNotFound) {
		return fmt.Errorf("find installed repository: %w", err)
	}
	if err != nil {
		return err
	}
	st, ok, err := app.agents.CheckSurface(args[0], doctor.SurfaceInput(root, binding.Project.ID))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no status for %q (want %s)", args[0], strings.Join(app.agents.CheckedSurfaces(), " or "))
	}
	width := 0
	for _, l := range st.Lines {
		width = max(width, len(l.Label))
	}
	for _, l := range st.Lines {
		fmt.Fprintf(cmd.OutOrStdout(), "%-*s %s\n", width+1, l.Label+":", l.Value)
	}
	return nil
}
