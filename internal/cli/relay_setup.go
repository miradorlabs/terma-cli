package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
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
			dir, err := daemon.Dir()
			if err != nil {
				return err
			}
			token, err := ensureRelayToken()
			if err != nil {
				return err
			}
			if addr == "" {
				addr = daemon.Addr(dir)
			}
			if err := config.WriteFileAtomic(filepath.Join(dir, daemon.AddrFile), []byte(addr+"\n"), 0o600); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			err = app.pointAgentsAtRelay(cmd.Context(), splitCommas(agents), addr, token, func(agent, detail string) {
				fmt.Fprintf(out, "%s exports to the relay at %s%s.\n", agent, addr, detail)
			}, func(note string) { fmt.Fprintln(out, note) })
			if err != nil {
				return err
			}
			fmt.Fprintln(out, "Hooks in the repositories your team collects claim their sessions; nothing else is forwarded.")
			// A running relay has the old address and token: replace it.
			daemon.Stop(dir)
			if _, ok := daemon.ServiceInstalled(); ok {
				// The service manager starts it again, with the new address and token.
			} else if !noStart {
				daemon.Spawn()
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "the loopback address the relay listens on (default "+claim.DefaultAddr+")")
	cmd.Flags().StringVar(&agents, "harness", strings.Join(app.agents.RelayTargets(app.availableAgentNames()), ","), "the agents to point at the relay")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "do not start the relay now (the next hook that claims a session will)")
	return cmd
}

func (app *App) pointAgentsAtRelay(ctx context.Context, selected []string, addr, token string, done func(agent, detail string), note func(string)) error {
	dir, err := daemon.Dir()
	if err != nil {
		return err
	}
	cfg := agents.RelayConfig{Endpoint: "http://" + addr, Token: token, HookCommand: termaHookCommand(), StateDir: dir}
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

func ensureRelayToken() (string, error) {
	if token, err := daemon.Token(); err == nil && token != "" {
		return token, nil
	}
	path, err := claim.TokenPath()
	if err != nil {
		return "", err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	return token, config.WriteFileAtomic(path, []byte(token+"\n"), 0o600)
}
