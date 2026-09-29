package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/harness"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

func newDesktopCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "desktop", Short: "Inspect repository-scoped Codex Desktop capture", Hidden: true}
	cmd.AddCommand(&cobra.Command{Use: "status", Short: "Show this repository's Desktop capture", RunE: statusDesktop})
	return cmd
}

func statusDesktop(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	binding, root, err := termaproject.ResolveDir(cwd)
	if errors.Is(err, termaproject.ErrNotFound) {
		return fmt.Errorf("find installed repository: %w", err)
	}
	if err != nil {
		return err
	}
	// Codex Desktop reads Codex's own global configuration, so its export is judged
	// exactly as the CLI's is.
	v := judgeHarness(gatherHarness(harness.Codex{}, binding.Project.ID, root), cfg.OTLPURL, relayEndpointHere(), binding.Project.ID)
	line, ready := statusAgent(v, true)
	fmt.Fprintf(out, "Repository:    %s\n", binding.Project.ID)
	fmt.Fprintf(out, "Export:        %s %s\n", yesNo(ready), line)
	if v.route == routeRelay {
		fmt.Fprintf(out, "Relay:         %s\n", relayCheck(ctx, []harnessVerdict{v}).Detail)
	}
	if st := v.status; st.Connected {
		fmt.Fprintf(out, "Prompt text:   %s\n", onOff(st.IncludePrompts))
		fmt.Fprintf(out, "Tool content:  %s\n", onOff(st.IncludeToolContent))
	}
	if codex, found := adapter.Lookup("codex"); found {
		if trusting, found := codex.(adapter.Trusting); found {
			trust, err := trusting.Trust(root)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Codex hooks:   %s\n", yesNo(trust.Trusted))
			if !trust.Trusted && trust.Fix != "" {
				fmt.Fprintf(out, "Next:          %s\n", trust.Fix)
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
