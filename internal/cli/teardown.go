package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func (app *App) newTeardownCommand() *cobra.Command {
	var signOut, assumeYes bool
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Undo terma setup on this machine: restore your agents and stop the relay",
		Long: `The machine-wide undo of ` + "`terma setup`" + `, safe to run again:

  1. Restores the settings terma changed in your coding agents (their telemetry
     exporters, status line and notifier) and removes their machine-wide hooks.
     The commit hooks terma added to repositories stay: with terma gone they only
     run the hook each one displaced.
  2. Stops the local relay, removes its background service, and deletes its state
     (its address and any telemetry not yet delivered). Its token is set aside, so
     the hooks of agents still running start nothing and send nothing.
  3. Deletes what the hooks kept on this machine: session records, cursors and any
     events not yet delivered, and the teams' collection policies, which the next
     setup fetches again.

Your sign-in is kept, so ` + "`terma setup`" + ` sets this machine up again in seconds,
with the relay's token as it was: agents still running keep reporting without a restart.
--sign-out also revokes the sign-in, and the next setup mints a new token. On a
machine set up with a server key, it stops the key being this machine's sign-in; the
key itself is revoked in the Terma web app.`,
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
			if err := app.undoSetup(cmd.Context(), out, !signOut); err != nil {
				return err
			}
			if signOut {
				if err := app.signOut(cmd); err != nil {
					return err
				}
			}
			fmt.Fprintln(out, "terma is torn down on this machine; `terma setup` sets it up again.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&signOut, "sign-out", false, "also revoke this machine's sign-in")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// undoSetup restores everything setup changed outside terma's own state and stops the
// relay for good; it leaves the sign-in, keys and repositories alone. keepToken keeps the
// relay's token aside for a setup under the same sign-in (daemon.RetireToken).
func (app *App) undoSetup(ctx context.Context, out io.Writer, keepToken bool) error {
	// Before the journals and the relay's token, which recognise what terma wrote, go.
	if err := app.releaseDeselected(ctx, nil, relayReport{
		ok:   func(label, what string) { fmt.Fprintf(out, "%s %s.\n", label, what) },
		then: func(step string) { fmt.Fprintln(out, step) },
	}); err != nil {
		return err
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

	// The machine-wide hooks point into the config directory, the relay service into the state directory, and both at this binary.
	say := func(what string) { fmt.Fprintln(out, strings.TrimSpace(what)) }
	if err := app.globalMode().Remove(say); err != nil {
		return fmt.Errorf("restore machine-wide hooks: %w", err)
	}
	_ = os.Remove(filepath.Join(app.dir, config.SetupDir)) // only once every record has gone
	// Without its token the relay refuses to run and hooks write no claims, so no hook can
	// start it again. It goes first, so nothing restarts the relay stopped below.
	if err := app.retireRelayToken(keepToken); err != nil {
		return fmt.Errorf("remove the relay's token: %w", err)
	}
	if removed, err := daemon.RemoveService(ctx, app.stateDir); err != nil {
		return fmt.Errorf("remove the relay service: %w", err)
	} else if removed {
		fmt.Fprintln(out, "Removed the relay service.")
	}
	dir := claim.Dir(app.stateDir)
	if _, err := os.Stat(dir); err == nil {
		daemon.Stop(dir)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove the relay's state: %w", err)
		}
		fmt.Fprintln(out, "Stopped the relay and removed its state.")
	}
	if err := app.removeRoutingState(); err != nil {
		return fmt.Errorf("remove routing state: %w", err)
	}
	// Only a warning: everything that had to happen in order has, and --sign-out still should.
	if err := app.removeHookState(); err != nil {
		fmt.Fprintf(out, "Warning: %v; run `terma teardown` again to finish.\n", err)
	}
	return nil
}

// removeRoutingState deletes every profile's routing/<profile>.json via CAS Delete (or
// DeleteCorrupt under the lock when a file is unreadable), retrying on a concurrent
// refresh or repair.
func (app *App) removeRoutingState() error {
	profiles, err := app.routingProfilesToRemove()
	if err != nil {
		return err
	}
	var errs []error
	for _, profile := range profiles {
		if derr := app.removeProfileRoutingState(profile); derr != nil {
			errs = append(errs, fmt.Errorf("profile %q: %w", profile, derr))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(app.stateDir, routing.StatesDir))
	return nil
}

func (app *App) routingProfilesToRemove() ([]string, error) {
	seen := map[string]struct{}{}
	add := func(name string) {
		if _, err := routing.StatePath(app.stateDir, name); err == nil {
			seen[name] = struct{}{}
		}
	}
	add(config.DefaultProfile)
	if cfg, err := app.loadConfig(); err == nil {
		add(cfg.ProfileName)
	}
	if file, err := config.LoadFile(app.dir); err == nil && file != nil {
		add(file.ActiveProfile)
		for name := range file.Profiles {
			add(name)
		}
	}
	entries, err := os.ReadDir(filepath.Join(app.stateDir, routing.StatesDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		add(strings.TrimSuffix(name, ".json"))
	}
	profiles := make([]string, 0, len(seen))
	for profile := range seen {
		profiles = append(profiles, profile)
	}
	slices.Sort(profiles)
	return profiles, nil
}

func (app *App) removeProfileRoutingState(profile string) error {
	for range 3 {
		st, found, err := routing.Load(app.stateDir, profile)
		if err != nil {
			err = routing.DeleteCorrupt(app.stateDir, profile)
		} else if !found {
			err = routing.ErrNoState
		} else {
			err = routing.Delete(app.stateDir, profile, st.Expect())
		}
		if err == nil || errors.Is(err, routing.ErrNoState) {
			return nil
		}
		if errors.Is(err, routing.ErrStaleGeneration) {
			continue
		}
		return err
	}
	return routing.ErrStaleGeneration
}

// removeHookState removes what hooks kept per session, which only they would prune, and
// the policies they ran under.
func (app *App) removeHookState() error {
	var errs []error
	for _, name := range append(app.hookStateDirs(), hookrun.AgentsDir, hookrun.PushesDir, termaproject.WorkspacesDir, spool.Dir, config.PoliciesDir) {
		errs = append(errs, os.RemoveAll(filepath.Join(app.stateDir, name)))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("remove the hooks' state: %w", err)
	}
	return nil
}

// retireRelayToken removes the relay's token, keeping it for this sign-in when keep.
func (app *App) retireRelayToken(keep bool) error {
	account, authURL := "", ""
	if !keep {
		if err := daemon.DiscardRetiredToken(app.stateDir); err != nil {
			return err
		}
	} else if cfg, err := app.loadConfig(); err == nil {
		account, authURL = daemon.Identity(cfg)
	}
	return daemon.RetireToken(app.stateDir, account, authURL)
}

var errNoSessionWithAPIKey = errors.New("TERMA_API_KEY is set — there is no session to sign out of; unset it to use the stored credential")

// signOut revokes this profile's session server-side and deletes the local credential,
// and an older terma's per-organization sign-ins with it. A failed revoke still clears the
// local file: the developer asked to be signed out. A profile set up with a server key stops
// counting it as its sign-in; the key itself is revoked only in the Terma web app.
func (app *App) signOut(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	cfg, err := app.loadConfig()
	if err != nil {
		return err
	}
	if cfg.APIKey != "" {
		return errNoSessionWithAPIKey
	}
	if cfg.ServerKeySignIn {
		if err := config.UpdateProfile(app.dir, cfg.ProfileName, func(p *config.Profile) { p.SetServerKeySignIn(false, "") }); err != nil {
			return err
		}
		fmt.Fprintln(out, "Signed out of the team's server key; it keeps working until you revoke it in the Terma web app.")
	}
	// Deleted under the credential lock, then revoked: once the stored copy is gone, no
	// refresh in another process (the relay's) presents its token, so the revoke never
	// sends a spent one, which would be reuse and end the whole family, a Terma Desktop
	// session it was delegated from included. No network call holds the lock.
	var cred *auth.Credential
	err = auth.Locked(cmd.Context(), app.dir, func(tx *auth.Tx) error {
		var err error
		if cred, err = tx.Load(cfg.ProfileName); err != nil {
			cred = nil
			return err
		}
		err = tx.SignOutIf(cfg.ProfileName, cred.RefreshToken)
		// Revoked only once the store no longer lists it: a copy still stored is one a
		// refresh may spend after the lock, and revoking a spent token is reuse.
		if still, lerr := auth.LoadIdentity(app.dir, cfg.ProfileName); (lerr == nil && still.SessionID == cred.SessionID) ||
			(lerr != nil && !errors.Is(lerr, auth.ErrNotLoggedIn)) {
			cred = nil
		}
		return err
	})
	if cred != nil {
		// Read, and gone from the store even when its keychain item could not be removed.
		if err := app.revokeSession(cmd.Context(), cfg, cred); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not revoke the session server-side (%v).\n", err)
		}
	}
	legacy := auth.HasLegacySignIns(app.dir)
	switch {
	case err == nil:
	case cred != nil && secret.IsUnavailable(err):
		return fmt.Errorf("signed out, but %w: delete the keychain items of the service %s by hand", err, secret.Service(app.dir))
	case cred != nil:
		return err
	case errors.Is(err, auth.ErrNotLoggedIn) && !legacy:
		if !cfg.ServerKeySignIn {
			fmt.Fprintln(out, "Already signed out.")
		}
		return nil
	case errors.Is(err, auth.ErrNotLoggedIn):
	case secret.IsUnavailable(err) || errors.Is(err, auth.ErrTokensUnreadable):
		// Tokens that cannot be read sign the machine out all the same: only the
		// server-side revoke needs them.
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not read the session to revoke it server-side (%v).\n", err)
		if err := auth.DeleteCredential(app.dir, cfg.ProfileName); secret.IsUnavailable(err) {
			return fmt.Errorf("signed out, but %w: delete the keychain items of the service %s by hand", err, secret.Service(app.dir))
		} else if err != nil {
			return err
		}
	default:
		return err
	}
	app.retireLegacySessions(cmd.Context(), cfg.AuthURL)
	fmt.Fprintln(out, "Signed out.")
	return nil
}
