package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

func newTelemetryDisconnectCommand() *cobra.Command {
	var assumeYes bool
	var scopeFlag string

	cmd := &cobra.Command{
		Use:    "disconnect <" + strings.Join(harness.Names(), "|") + ">",
		Short:  "Stop a harness exporting to Terma",
		Hidden: true,
		Long: `Removes the telemetry settings Terma wrote, and nothing else.

The server key stays live — it is bound to the project, not to this machine, and may
be in use elsewhere. Revoke it in the web app when you are done with it; the key's
masked prefix is printed so you can find it in the list.

--scope local removes the repository's own policy from its .claude/settings.json
instead, and leaves your global connect as it is.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scope, err := harness.ParseScope(scopeFlag)
			if err != nil {
				return err
			}
			h, err := harness.Lookup(args[0])
			if err != nil {
				return err
			}
			if scope == harness.ScopeLocal {
				if h, err = localHarness(cmd.Context(), h); err != nil {
					return err
				}
			}
			out := cmd.OutOrStdout()

			st, err := h.Status()
			if err != nil {
				return err
			}
			// Terma's Codex notifier lives outside the telemetry key set, so it can
			// linger after the keys are gone (removed by hand or by an older CLI). That
			// still needs restoring, so it counts as work to do.
			codexNotifierLeftover := false
			if h.Name() == "codex" && scope == harness.ScopeGlobal {
				if ns, nerr := (harness.Codex{}).CodexNotify(); nerr == nil {
					codexNotifierLeftover = ns.Terma
				}
			}
			// Repository install can wrap the user-level status line without a
			// global telemetry connection. It still belongs to this disconnect.
			claudeStatusLineLeftover := false
			if h.Name() == "claude" && scope == harness.ScopeGlobal {
				if line, lineErr := (harness.Claude{}).StatusLineState(""); lineErr == nil {
					claudeStatusLineLeftover = line.Installed || line.Replaced
				}
			}
			// Keyed off the settings actually present, not off Connected. A config with
			// telemetry switched off, or with the endpoint deleted, is not "connected" —
			// but it still has Terma's server key sitting in it, and that is the state
			// where walking away would be worst.
			if st.ManagedKeys == 0 && !codexNotifierLeftover && !claudeStatusLineLeftover {
				fmt.Fprintf(out, "%s has no Terma telemetry settings%s. Nothing to do.\n", h.DisplayName(), scopeSuffix(scope))
				return nil
			}
			// A local layer has no endpoint and is never "connected"; the line is for
			// the global file, where it names a real state.
			if !st.Connected && scope == harness.ScopeGlobal {
				fmt.Fprintf(out, "%s is not exporting, but Terma settings are still present.\n\n", h.DisplayName())
			}

			fmt.Fprintf(out, "This will remove Terma's settings from:\n  %s\n", st.ConfigPath)
			if st.KeyPrefix != "" {
				fmt.Fprintf(out, "\nThe server key %s stays live — revoke it in the web app.\n", st.KeyPrefix)
			}
			fmt.Fprintln(out)

			if !assumeYes {
				question := fmt.Sprintf("Disconnect %s?", h.DisplayName())
				if scope == harness.ScopeLocal {
					question = fmt.Sprintf("Remove this repository's telemetry settings for %s?", h.DisplayName())
				}
				ok, err := confirm(cmd, question)
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(out, "Cancelled. Nothing was changed.")
					return nil
				}
			}

			result, err := h.Disconnect()
			if err != nil {
				return err
			}
			if h.Name() == "claude" && scope == harness.ScopeGlobal {
				switch restored, err := (harness.Claude{}).RemoveStatusLine(); {
				case err != nil:
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not restore the status line (%v).\n", err)
				case restored:
					fmt.Fprintln(out, "Status line: restored to what it was before terma wrapped it.")
				}
			}
			if h.Name() == "codex" && scope == harness.ScopeGlobal {
				switch restored, err := (harness.Codex{}).RemoveCodexNotify(); {
				case err != nil:
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not restore the previous Codex notifier (%v).\n", err)
				case restored:
					fmt.Fprintln(out, "Notifier: restored to what ran before terma connected.")
				}
			}

			if scope == harness.ScopeLocal {
				fmt.Fprintf(out, "\nRemoved %d setting(s) from %s.\n", result.Removed, st.ConfigPath)
			} else {
				fmt.Fprintf(out, "\nDisconnected. Removed %d setting(s) from %s.\n", result.Removed, st.ConfigPath)
			}
			if result.Restored > 0 {
				fmt.Fprintf(out, "Restored %d setting(s) to the value held before Terma connected.\n", result.Restored)
			}
			if len(result.Skipped) > 0 {
				// Changed after the connect, so they are somebody's deliberate edit and
				// not Terma's to throw away.
				fmt.Fprintf(out, "Left alone (changed since connecting): %s\n", strings.Join(result.Skipped, ", "))
			}
			if result.Unjournaled {
				fmt.Fprintf(out, "No record of the original values survived, so they were removed rather than restored.\n")
			}
			fmt.Fprintf(out, "Restart %s for it to take effect.\n", h.DisplayName())
			if scope == harness.ScopeLocal {
				fmt.Fprintln(out, "Commit the change if the file is committed.")
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().StringVar(&scopeFlag, "scope", "", "which layer to remove: global (your user settings, default) or local (this repository's .claude/settings.json)")
	return cmd
}
