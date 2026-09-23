package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/prompt"
	"github.com/miradorlabs/terma-cli/internal/style"
)

type setupFlags struct {
	harnesses string
	noBrowser bool
	assumeYes bool
}

func newSetupCommand() *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Sign in and choose your coding agents (once per developer)",
		Long: `Gets this machine ready to use terma. It does two things and nothing more —
no project, no telemetry configuration, no files touched:

  1. Signs you in (a browser handoff; --no-browser prints the URL instead).
  2. Records which coding agents you work with (Claude Code or Codex), so ` + "`terma install`" + ` never has to ask again.

setup is optional: ` + "`terma install`" + ` signs you in and asks for your agents itself
when you have not run it. The real configuration — pointing an agent at a project,
wiring the hooks — happens per repository, in ` + "`terma install`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runSetup(cmd, f) },
	}
	cmd.Flags().StringVar(&f.harnesses, "harness", "", "comma-separated agents to record ("+strings.Join(availableAgentNames(), ", ")+"); default: a picker")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser")
	cmd.Flags().BoolVarP(&f.assumeYes, "yes", "y", false, "skip the picker; record every available installed agent")
	return cmd
}

func runSetup(cmd *cobra.Command, f setupFlags) error {
	out := cmd.OutOrStdout()
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// A welcome for a person watching: the logo, and beside it who and where terma is
	// pointed right now. Suppressed off a terminal (a pipe, an agent, NO_COLOR), like the
	// spinner.
	if p := style.For(out); p.Enabled() {
		fmt.Fprintln(out, style.Header(p, setupHeaderInfo(cfg, p)))
	}
	if cfg.APIKey != "" {
		return errors.New("TERMA_API_KEY is set — setup signs in as a person; unset it first")
	}

	// 1. Sign in — reusing the session this machine already has, verified.
	if cfg, err = signInAndReload(cmd, cfg, signInOptions{noBrowser: f.noBrowser}); err != nil {
		return err
	}

	// 2. Which agents this developer uses. A machine-level preference, not a connection.
	fmt.Fprintln(out)
	names, err := chooseHarnesses(cmd, cfg, f)
	if errors.Is(err, errCancelled) {
		fmt.Fprintln(out, "Cancelled. Nothing was recorded.")
		return nil
	}
	if err != nil {
		return err
	}
	if err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) { p.Harnesses = names }); err != nil {
		return err
	}

	if len(names) == 0 {
		fmt.Fprintln(out, "\nNo agents recorded. `terma install` will ask you to pick some in each repository.")
	} else {
		fmt.Fprintf(out, "\nAgents recorded: %s.\n", joinNames(adapterDisplayNames(names)))
	}
	fmt.Fprintf(out, "\n%s Now run `terma install` in each codebase you want to instrument with terma.\n",
		style.For(out).Bold("Done!"))
	return nil
}

// chooseHarnesses resolves the machine-level agent list: the --harness flag if given,
// else a checkbox picker with coming-soon agents disabled (preselecting available
// agents already recorded or detected), else — with --yes or no terminal — the
// available recorded or detected agents.
func chooseHarnesses(cmd *cobra.Command, cfg *config.Config, f setupFlags) ([]string, error) {
	if strings.TrimSpace(f.harnesses) != "" {
		return parseAgentList(f.harnesses)
	}
	ctx := cmd.Context()
	preselect := map[string]bool{}
	for _, n := range cfg.Harnesses {
		preselect[n] = true
	}
	for _, n := range detectedAgents(ctx) {
		preselect[n] = true
	}
	if f.assumeYes || !canPrompt() {
		return selectedInRegistryOrder(preselect), nil
	}

	form := harnessSelectionForm(ctx, preselect)
	result, err := prompt.Run(form)
	if errors.Is(err, prompt.ErrCancelled) {
		return nil, errCancelled
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for i, a := range harnessSelectionAgents() {
		if agentAvailable(a.Name()) && result[i].Selected {
			names = append(names, a.Name())
		}
	}
	return names, nil
}

// agentAvailable gates onboarding while the remaining integrations are coming soon.
func agentAvailable(name string) bool {
	return name == "claude" || name == "codex"
}

func availableAgentNames() []string {
	var names []string
	for _, a := range adapter.All() {
		if agentAvailable(a.Name()) {
			names = append(names, a.Name())
		}
	}
	return names
}

// harnessSelectionAgents puts available agents first, preserving registry order
// within each group. Both the form and its result mapping must use this order.
func harnessSelectionAgents() []adapter.Adapter {
	var agents []adapter.Adapter
	for _, available := range []bool{true, false} {
		for _, a := range adapter.All() {
			if agentAvailable(a.Name()) == available {
				agents = append(agents, a)
			}
		}
	}
	return agents
}

func harnessSelectionForm(ctx context.Context, preselect map[string]bool) *prompt.Form {
	form := &prompt.Form{Title: "Which coding agents do you use? (space toggles, enter confirms)"}
	for _, a := range harnessSelectionAgents() {
		item := prompt.Item{Label: a.DisplayName(), Kind: prompt.Check}
		if agentAvailable(a.Name()) {
			item.Detail = agentDetail(ctx, a.Name())
			item.Selected = preselect[a.Name()]
		} else {
			item.Disabled = true
			item.Reason = "Coming Soon"
		}
		form.Items = append(form.Items, item)
	}
	return form
}

// parseAgentList validates a comma-separated list of adapter names.
func parseAgentList(raw string) ([]string, error) {
	seen := map[string]bool{}
	for _, n := range splitCommas(raw) {
		if _, ok := adapter.Lookup(n); !ok {
			return nil, fmt.Errorf("unknown agent %q (want %s)", n, joinNames(availableAgentNames()))
		}
		if !agentAvailable(n) {
			return nil, fmt.Errorf("agent %q: Coming Soon; available agents: %s", n, joinNames(availableAgentNames()))
		}
		seen[n] = true
	}
	return selectedInRegistryOrder(seen), nil
}

// selectedInRegistryOrder returns available chosen adapter names in registry order.
func selectedInRegistryOrder(chosen map[string]bool) []string {
	var names []string
	for _, a := range adapter.All() {
		if agentAvailable(a.Name()) && chosen[a.Name()] {
			names = append(names, a.Name())
		}
	}
	return names
}

// detectedAgents lists the supported agents whose binary is present on this machine.
func detectedAgents(ctx context.Context) []string {
	var names []string
	for _, a := range adapter.All() {
		if agentAvailable(a.Name()) && a.Installed(ctx) {
			names = append(names, a.Name())
		}
	}
	return names
}

func agentDetail(ctx context.Context, name string) string {
	if a, ok := adapter.Lookup(name); ok && a.Installed(ctx) {
		return "installed"
	}
	return ""
}

// adapterDisplayNames maps adapter tokens to their display names, in the given order.
func adapterDisplayNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if a, ok := adapter.Lookup(n); ok {
			out = append(out, a.DisplayName())
		} else {
			out = append(out, n)
		}
	}
	return out
}

// lookPathAny reports whether any of the given binaries is on PATH.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// setupHeaderInfo is the column beside the logo: the product, who terma is signed in
// as right now (and where it is pointed), and the working directory. It reads only local
// state — no network — so it is safe to show before signing in.
func setupHeaderInfo(cfg *config.Config, p style.Palette) []string {
	info := []string{p.Bold("Terma CLI") + " " + p.Dim("("+Version+")")}

	switch {
	case cfg.APIKey != "":
		info = append(info, p.Dim("using TERMA_API_KEY"))
	default:
		cred, err := auth.LoadCredential(cfg.ProfileName)
		if err != nil {
			info = append(info, p.Dim("not signed in — setup will sign you in"))
			break
		}
		who := firstNonEmpty(cred.UserEmail, "signed in")
		org := firstNonEmpty(cfg.OrganizationName, cfg.OrganizationID)
		if org != "" {
			who += "  " + p.Dim("("+org+")")
		}
		info = append(info, who)
	}

	if cwd, err := os.Getwd(); err == nil {
		info = append(info, p.Dim(tildePath(cwd)))
	}
	return info
}

// tildePath abbreviates the home directory to ~, as a shell prompt would.
func tildePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}
