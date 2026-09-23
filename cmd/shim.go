package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/shim"
)

// newShimCommand groups the internal per-repo routing entry points. `prepare` is what a
// PATH shim or an opt-in shell wrapper calls before it execs the agent; `uninstall`
// tears down per-repo routing machine-wide (the PATH shims, routing records, and
// per-project Claude settings).
func newShimCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "shim",
		Short:  "Per-repository agent routing (internal)",
		Hidden: true,
	}
	cmd.AddCommand(newShimPrepareCommand(), newShimExecCommand(), newShimUninstallCommand(), newShimStatusCommand())
	return cmd
}

// newShimExecCommand runs the real agent binary with per-repo routing applied. Flag
// parsing is disabled so the agent's own flags pass through untouched.
func newShimExecCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "exec <agent> [-- args...]",
		Short:              "Run an agent with this repository's Terma project applied",
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
		Short: "Remove per-repo routing from this machine (shims, routing records, Claude settings)",
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

// Preparation never starts the agent. The shell retains responsibility for execution.
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
