package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/shim"
)

// newShimCommand is what is left of per-repository routing through PATH shims, which
// the local relay replaced: `uninstall` removes the shims an earlier build installed,
// and `prepare` and `exec` keep the scripts still on a machine starting the real agent,
// quietly, until they are gone (`terma update --refresh` removes them too).
func newShimCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "shim",
		Short:  "Remove the PATH shims earlier builds installed (internal)",
		Hidden: true,
	}
	cmd.AddCommand(newShimPrepareCommand(), newShimExecCommand(), newShimUninstallCommand())
	return cmd
}

// newShimExecCommand is the oldest shim scripts' entry point: the real agent, unchanged.
func newShimExecCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "exec <agent> [-- args...]",
		Short:              "Run an agent (what an earlier build's shim calls)",
		Hidden:             true,
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rest := args[1:]
			if len(rest) > 0 && rest[0] == "--" {
				rest = rest[1:]
			}
			return shim.Exec(args[0], rest)
		},
	}
}

func newShimUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the PATH shims and their PATH block from this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			removed, err := shim.RemoveLegacy()
			if err != nil {
				return err
			}
			if !removed {
				fmt.Fprintln(cmd.OutOrStdout(), "No terma PATH shims on this machine.")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Removed terma's PATH shims, and the block that put them on PATH.")
			if rc, ok := shim.ShellRC(); ok {
				fmt.Fprintf(cmd.OutOrStdout(), "Run `%s` or open a new terminal so this shell forgets them.\n", reloadCommand(tildePath(rc.Path)))
			}
			return nil
		},
	}
}

// newShimPrepareCommand is what the later shim scripts call before they start the agent:
// an empty plan, so they start it as it is.
func newShimPrepareCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "prepare <agent> <directory> [-- args...]",
		Hidden:             true,
		Args:               cobra.MinimumNArgs(2),
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return shim.PrepareNothing(args[1])
		},
	}
}
