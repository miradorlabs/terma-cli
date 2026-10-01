package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

func (app *App) newPauseCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "pause",
		Short:  "Stop capturing agent sessions on this machine",
		Long:   "Hooks and the local relay capture nothing new until `terma resume`; events already\nqueued are still delivered. In global mode only an organization that lets members\npause allows it.",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !hookPolicy().PauseAllowed() {
				return errors.New("your organization collects every session on this machine and does not let members pause it")
			}
			path, err := config.PausedPath()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := config.WriteFileAtomic(path, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
				return err
			}
			fmt.Fprintln(style.Highlight(cmd.OutOrStdout()), "Paused: hooks and the relay capture nothing new on this machine, and commits are not stamped. Events already queued are still delivered. `terma resume` starts capture again.")
			return nil
		},
	}
}

func (app *App) newResumeCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "resume",
		Short:  "Start capturing agent sessions again after `terma pause`",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.PausedPath()
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Resumed: capture is on again.")
			return nil
		},
	}
}

// capturePaused reports whether this machine is paused and its organization allows it.
func capturePaused() bool { return config.Paused() && hookPolicy().PauseAllowed() }

// captureNotice is why nothing is being collected, when something stops it: a pause, or a
// policy that has gone unrefreshed past config.MaxPolicyAge.
func captureNotice() string {
	if capturePaused() {
		return "Capture is paused on this machine, so commits are not stamped — run `terma resume` to start it again."
	}
	if cfg, err := config.Load(config.Overrides{}); err == nil && cfg.Policy.Validated() && cfg.Policy.Expired(time.Now()) {
		return "Your organization's collection policy has not been refreshed for over a week, so nothing is collected — run `terma setup` to sign in again."
	}
	return ""
}
