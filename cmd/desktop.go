package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

const (
	desktopServiceLabel   = "ai.terma.codex-relay"
	legacyDesktopBaseURL  = "http://127.0.0.1:43199"
)

func newDesktopCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "desktop", Short: "Inspect repository-scoped Codex Desktop capture", Hidden: true}
	cmd.AddCommand(&cobra.Command{Use: "status", Short: "Show this repository's Desktop capture", RunE: statusDesktop})
	cmd.AddCommand(&cobra.Command{Use: "disconnect", Short: "Remove a previously installed Desktop relay", RunE: disconnectDesktop})
	return cmd
}

// removeLegacyDesktopRelay only alters the exporter when it still points at Terma's
// old loopback endpoint. An unrelated global Codex exporter belongs to the user.
func removeLegacyDesktopRelay(cmd *cobra.Command) (bool, error) {
	status, err := (harness.Codex{}).Status()
	if err != nil {
		return false, err
	}
	changed := false
	if status.Endpoint == legacyDesktopBaseURL {
		if _, err := (harness.Codex{}).Disconnect(); err != nil {
			return false, err
		}
		changed = true
	}
	path, err := desktopServicePath()
	if err != nil {
		return changed, err
	}
	if _, err := os.Stat(path); err == nil {
		if current, err := user.Current(); err == nil && filepath.Dir(filepath.Dir(filepath.Dir(path))) == current.HomeDir {
			target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + desktopServiceLabel
			_ = exec.CommandContext(cmd.Context(), "launchctl", "bootout", target).Run()
		}
		if err := os.Remove(path); err != nil {
			return changed, err
		}
		changed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return changed, err
	}
	return changed, nil
}

func disconnectDesktop(cmd *cobra.Command, _ []string) error {
	changed, err := removeLegacyDesktopRelay(cmd)
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintln(cmd.OutOrStdout(), "Previous Desktop relay removed. Restart Codex Desktop to drop the old exporter.")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "No previous Desktop relay is installed.")
	}
	return nil
}

func desktopServicePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", desktopServiceLabel+".plist"), nil
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
	root, err := termaproject.Find(cwd)
	if err != nil {
		return fmt.Errorf("find installed repository: %w", err)
	}
	binding, err := termaproject.Load(root)
	if err != nil {
		return err
	}
	projectID := binding.Project.ID
	route, ok, err := shim.LoadRecord(projectID)
	if err != nil {
		return err
	}
	ready := ok && route.Desktop != nil && *route.Desktop && slices.Contains(route.Signals, "logs") &&
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
