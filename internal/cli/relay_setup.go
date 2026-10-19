package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// termaHookCommand is how an extension terma writes reaches `terma hook`: by this binary's
// absolute path, since a desktop-started agent has the system PATH.
func termaHookCommand() []string {
	if exe, err := procinfo.AbsExecutable(); err == nil {
		return []string{exe, "hook"}
	}
	return []string{"terma", "hook"}
}

func (app *App) newRelaySetupCommand() *cobra.Command {
	var addr, agents string
	var noStart bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Point the agents' global exporters at the local relay",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := daemon.Dir(app.stateDir)
			if err != nil {
				return err
			}
			// Only an address asked for is recorded: writing back the one read would undo a
			// relay's move made meanwhile.
			if addr != "" {
				if err := daemon.RecordAddr(dir, addr); err != nil {
					return err
				}
			}
			if _, err := app.ensureRelayToken(); err != nil {
				return err
			}
			// One pair, never a moving relay's new token with its old address.
			current, token, err := daemon.Current(app.stateDir)
			if err != nil {
				return err
			}
			addr = current
			out := cmd.OutOrStdout()
			err = app.withExporters(cmd.Context(), func() error {
				return app.pointAgentsAtRelay(cmd.Context(), splitCommas(agents), addr, token, func(agent, detail string) {
					fmt.Fprintf(out, "%s exports to the relay at %s%s.\n", agent, addr, detail)
				}, func(note string) { fmt.Fprintln(out, note) })
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(out, "Hooks in the repositories your team collects claim their sessions; nothing else is forwarded.")
			// A running relay has the old address and token: replace it.
			daemon.Stop(dir)
			if noStart {
				return nil
			}
			// The service's relay, or failing that one on demand, with the new address and token.
			if _, ok := daemon.ServiceInstalled(app.stateDir); !ok || daemon.RestartService(cmd.Context(), app.stateDir) != nil {
				daemon.Spawn(app.stateDir, app.version)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "the loopback address the relay listens on (default "+claim.DefaultAddr+")")
	cmd.Flags().StringVar(&agents, "harness", strings.Join(app.agents.RelayTargets(app.availableAgentNames()), ","), "the agents to point at the relay")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "do not start the relay now (the next hook that claims a session will)")
	return cmd
}

// withExporters runs fn holding daemon.ExportersLockFile, so setup, teardown and a relay
// that moved change the agents' relay exporters one at a time; whoever chooses which to
// change chooses under it too. While another holds it past exportersLockWait, or it cannot
// be had at all, fn does not run.
func (app *App) withExporters(ctx context.Context, fn func() error) error {
	dir, err := daemon.Dir(app.stateDir) // the lock's home, before a first setup too
	if err != nil {
		return err
	}
	lockCtx, cancel := context.WithTimeout(ctx, exportersLockWait)
	unlock, err := flock.Lock(lockCtx, filepath.Join(dir, daemon.ExportersLockFile))
	cancel()
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return errors.New("another terma is changing the agents' exporters; try again in a moment")
	case err != nil:
		return fmt.Errorf("lock the agents' exporters: %w", err)
	}
	defer unlock()
	return fn()
}

// pointAgentsAtRelay points the selected agents' exporters at the relay on addr; callers
// hold withExporters.
func (app *App) pointAgentsAtRelay(ctx context.Context, selected []string, addr, token string, done func(agent, detail string), note func(string)) error {
	cfg := agents.RelayConfig{Endpoint: "http://" + addr, Token: token, HookCommand: termaHookCommand()}
	for _, name := range selected {
		integration, ok := app.agents.Find[agents.RelayExporter](name)
		if !ok {
			return fmt.Errorf("unknown relay exporter %q (choose %v)", name, app.agents.RelayTargets(app.agents.Names()))
		}
		result, err := integration.ConfigureRelay(ctx, cfg)
		if err != nil {
			return fmt.Errorf("%s: %w", integration.DisplayName(), err)
		}
		if !result.Pending {
			paths := make([]string, 0, len(result.Paths))
			for _, path := range result.Paths {
				paths = append(paths, output.TildePath(path))
			}
			detail := ""
			if len(paths) > 0 {
				detail = " (" + strings.Join(paths, ", ") + ")"
			}
			done(integration.DisplayName(), detail)
		}
		for _, text := range result.Notes {
			note(text)
		}
	}
	return nil
}

// exportersLockWait bounds the wait for daemon.ExportersLockFile.
const exportersLockWait = 30 * time.Second

// ensureRelayToken is the relay's token: the one there is, else the one teardown kept for
// this sign-in, which agents still running present, else a new one.
func (app *App) ensureRelayToken() (string, error) {
	if token, err := daemon.Token(app.stateDir); err == nil && token != "" {
		return token, nil
	}
	if cfg, err := app.loadConfig(); err == nil {
		org, authURL := daemon.Identity(cfg)
		if token, ok := daemon.ReviveToken(app.stateDir, org, authURL); ok {
			return token, nil
		}
	}
	token, err := daemon.NewToken()
	if err != nil {
		return "", err
	}
	return token, config.WriteFileAtomic(claim.TokenPath(app.stateDir), []byte(token+"\n"), 0o600)
}
