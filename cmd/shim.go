package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/shim"
)

// newShimCommand groups what is left of per-repository routing, which terma no longer
// does. `prepare` and `exec` answer a PATH shim or shell wrapper an earlier terma
// installed, starting the agent unchanged; `uninstall` removes all of it (the PATH shims
// and the block in the shell's startup file, routing records, per-project Claude
// settings), as setup, install and a refresh also do.
func newShimCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "shim",
		Short:  "Remove the per-repository routing earlier versions installed (internal)",
		Hidden: true,
	}
	cmd.AddCommand(newShimPrepareCommand(), newShimExecCommand(), newShimUninstallCommand(), newShimStatusCommand())
	return cmd
}

// newShimExecCommand runs the real agent binary, unchanged. Flag parsing is disabled so
// the agent's own flags pass through untouched.
func newShimExecCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "exec <agent> [-- args...]",
		Short:              "Run an agent (what an old shim calls)",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			agent := args[0]
			rest := args[1:]
			if len(rest) > 0 && rest[0] == "--" {
				rest = rest[1:]
			}
			// On Unix this replaces the process and never returns; an error means the
			// agent binary could not be found or executed.
			return shim.Exec(agent, rest)
		},
	}
}

func newShimUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the per-repository routing earlier versions installed (shims, routing records, Claude settings)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := shim.RemoveAll(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Removed the terma PATH shims, routing records, and per-project Claude settings.")
			if rc, ok := shim.ShellRC(); ok {
				fmt.Fprintf(cmd.OutOrStdout(), "Took terma's PATH block out of %s, if it was there. A PATH line you added by hand is yours to remove.\n", tildePath(rc.Path))
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Remove the shim directory from your PATH in your shell's startup file if you added it.")
			}
			return nil
		},
	}
}

func newShimStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the per-repo routing shim directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := shim.ShimBinDir()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "PATH shim directory: %s\n", dir)
			return nil
		},
	}
}

// Preparation never starts the agent: an old shim asks it for arguments to add, and the
// answer is none.
func newShimPrepareCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "prepare <agent> <directory> [-- args...]",
		Hidden:             true,
		Args:               cobra.MinimumNArgs(2),
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rest := args[2:]
			if len(rest) > 0 && rest[0] == "--" {
				rest = rest[1:]
			}
			return shim.Prepare(args[0], args[1], rest)
		},
	}
}
