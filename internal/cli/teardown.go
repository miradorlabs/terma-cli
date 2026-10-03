package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

func (app *App) newTeardownCommand() *cobra.Command {
	var signOut, assumeYes bool
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Undo terma setup on this machine: restore your agents and stop the relay",
		Long: `The machine-wide undo of ` + "`terma setup`" + `, safe to run again:

  1. Restores the settings terma changed in your coding agents (their telemetry
     exporters, status line and notifier) and removes machine-wide hooks.
  2. Stops the local relay, removes its background service, and deletes its state
     (its token, its address and any telemetry not yet delivered), so no hook
     starts it again.

Your sign-in is kept, so ` + "`terma setup`" + ` sets this machine up again in seconds;
--sign-out also revokes it. Repositories keep their committed hooks and binding,
which everyone who works in them shares: ` + "`terma uninstall`" + ` inside one removes them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Refused before anything changes, so teardown never stops halfway.
			if signOut {
				cfg, err := app.loadConfig()
				if err != nil {
					return err
				}
				if cfg.APIKey != "" {
					return errNoSessionWithAPIKey
				}
			}
			if !assumeYes {
				question := "Restore your agents' settings and stop terma's relay on this machine?"
				if signOut {
					question = "Restore your agents' settings, stop terma's relay and sign out on this machine?"
				}
				ok, err := confirm(cmd, question)
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(cmd.OutOrStdout(), "Cancelled. Nothing was changed.")
					return nil
				}
			}
			out := cmd.OutOrStdout()
			if err := app.undoSetup(cmd.Context(), out); err != nil {
				return err
			}
			if signOut {
				if err := app.signOut(cmd); err != nil {
					return err
				}
			}
			fmt.Fprintln(out, "terma is torn down on this machine; `terma setup` sets it up again.")
			fmt.Fprintln(out, "Repositories keep their committed hooks and binding; remove one with `terma uninstall` inside it.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&signOut, "sign-out", false, "also revoke this machine's sign-in")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// undoSetup restores everything setup changed outside terma's own state and stops the
// relay for good; it leaves the sign-in, keys and repositories alone.
func (app *App) undoSetup(ctx context.Context, out io.Writer) error {
	// Restore what Terma displaced before anything that holds the journals goes.
	for _, h := range app.agents.Harnesses() {
		result, err := h.Disconnect()
		if err != nil {
			return fmt.Errorf("restore %s settings: %w", h.DisplayName(), err)
		}
		if result.Removed+result.Restored > 0 {
			fmt.Fprintf(out, "Restored %s settings.\n", h.DisplayName())
		}
	}
	for _, s := range app.agents.With[agents.StatusLiner]() {
		if restored, err := s.RemoveStatusLine(); err != nil {
			return fmt.Errorf("restore %s status line: %w", s.DisplayName(), err)
		} else if restored {
			fmt.Fprintf(out, "Restored the %s status line.\n", s.DisplayName())
		}
	}
	for _, n := range app.agents.With[agents.Notifier]() {
		if restored, err := n.RemoveNotifier(); err != nil {
			return fmt.Errorf("restore %s notifier: %w", n.DisplayName(), err)
		} else if restored {
			fmt.Fprintf(out, "Restored the %s notifier.\n", n.DisplayName())
		}
	}

	// The machine-wide hooks and the relay service point into the config directory and at this binary.
	say := func(what string) { fmt.Fprintln(out, strings.TrimSpace(what)) }
	if err := app.globalMode().Remove(ctx, say); err != nil {
		return fmt.Errorf("restore machine-wide hooks: %w", err)
	}
	if removed, err := daemon.RemoveService(ctx); err != nil {
		return fmt.Errorf("remove the relay service: %w", err)
	} else if removed {
		fmt.Fprintln(out, "Removed the relay service.")
	}
	// Without its token the relay refuses to run and hooks write no claims, so a hook in a
	// bound repository cannot start it again; setup writes a new one.
	dir, err := claim.Dir()
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err == nil {
		daemon.Stop(dir)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove the relay's state: %w", err)
		}
		fmt.Fprintln(out, "Stopped the relay and removed its state.")
	}
	return nil
}

var errNoSessionWithAPIKey = errors.New("TERMA_API_KEY is set — there is no session to sign out of; unset it to use the stored credential")

// signOut revokes every session this profile holds server-side and deletes the local
// credentials, one per organization signed into. A failed revoke still clears the local
// file: the developer asked to be signed out.
func (app *App) signOut(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	cfg, err := app.loadConfig()
	if err != nil {
		return err
	}
	if cfg.APIKey != "" {
		return errNoSessionWithAPIKey
	}
	creds, err := auth.Credentials(cfg.ProfileName)
	if err != nil {
		return err
	}
	if len(creds) == 0 {
		fmt.Fprintln(out, "Already signed out.")
		return nil
	}
	for _, cred := range creds {
		if err := app.revokeSession(cmd.Context(), cfg, cred); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not revoke the session for %s server-side (%v).\n",
				cmp.Or(cred.OrganizationID, "this organization"), err)
		}
	}
	if err := auth.DeleteCredential(cfg.ProfileName); err != nil {
		return err
	}
	if len(creds) > 1 {
		fmt.Fprintf(out, "Signed out of %d organizations.\n", len(creds))
	} else {
		fmt.Fprintln(out, "Signed out.")
	}
	return nil
}
