package cmd

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

func newDesktopCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "desktop", Short: "Inspect repository-scoped Codex Desktop capture", Hidden: true}
	cmd.AddCommand(&cobra.Command{Use: "status", Short: "Show this repository's Desktop capture", RunE: statusDesktop})
	return cmd
}

func statusDesktop(cmd *cobra.Command, _ []string) error {
	global, err := (harness.Codex{}).Status()
	if err != nil {
		return err
	}
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
	projectID := binding.Project.ID
	route, ok, err := shim.LoadRecord(projectID)
	if err != nil {
		return err
	}
	ready := ok && route.Desktop && slices.Contains(route.Signals, "logs") &&
		slices.Contains(route.Harnesses, shim.AgentCodex) && keystore.GetFor(shim.AgentCodex, projectID) != ""
	fmt.Fprintf(cmd.OutOrStdout(), "Repository:    %s\n", projectID)
	fmt.Fprintf(cmd.OutOrStdout(), "Desktop route: %s\n", yesNo(ready))
	if global.Connected {
		fmt.Fprintln(cmd.OutOrStdout(), "Global export: on (may include other repositories)")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "Global export: off")
	}
	if ok {
		fmt.Fprintf(cmd.OutOrStdout(), "Prompt text:   %s\n", onOff(route.IncludePrompts))
		fmt.Fprintf(cmd.OutOrStdout(), "Tool content:  %s\n", onOff(route.IncludeToolContent))
	}
	if codex, found := adapter.Lookup("codex"); found {
		if trusting, found := codex.(adapter.Trusting); found {
			trust, err := trusting.Trust(root)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Codex hooks:   %s\n", yesNo(trust.Trusted))
			if !trust.Trusted && trust.Fix != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Next:          %s\n", trust.Fix)
			}
		}
	}
	return nil
}

func yesNo(v bool) string {
	if v {
		return "ready"
	}
	return "not ready"
}
