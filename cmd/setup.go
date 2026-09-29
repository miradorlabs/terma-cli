package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/prompt"
	"github.com/miradorlabs/terma-cli/internal/spinner"
	"github.com/miradorlabs/terma-cli/internal/style"
)

type setupFlags struct {
	harnesses  string
	noBrowser  bool
	assumeYes  bool
	projectRef string
	noRelay    bool
	// The export choices, recorded machine-wide.
	signals            string
	prompts            string
	excludeToolContent bool
	identity           string
	noStatusLine       bool
	force              bool
	verbose            bool
}

const codexDesktopAgent = "codex-desktop"

// agentChoice is an onboarding surface. Codex CLI and Codex desktop share one
// repository adapter, but developers choose independently how they launch it.
type agentChoice struct {
	name      string
	display   string
	installed func(context.Context) bool
}

func (a agentChoice) Name() string        { return a.name }
func (a agentChoice) DisplayName() string { return a.display }
func (a agentChoice) Installed(ctx context.Context) bool {
	return a.installed(ctx)
}

func newSetupCommand() *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Sign in, choose your coding agents and configure their telemetry (once per machine)",
		Long: `Gets this machine ready to use terma:

  1. Signs you in (a browser handoff; --no-browser prints the URL instead).
  2. Records which coding agents you work with (including Codex CLI and Codex
     desktop separately), so ` + "`terma install`" + ` never has to ask again.
  3. Picks this machine's project (--project names it): where sessions in a
     repository that is not bound to a project report.
  4. Configures each agent's telemetry once, machine-wide, in its own global
     settings file: every launcher — the CLI, Codex Desktop, Claude Desktop —
     reads it. The agents export to terma's relay on this machine, which sends
     each session to the project of the repository it ran in; --no-relay (or a
     platform without launchd or systemd) exports straight to Terma instead,
     every session to this machine's project. Prompt text and model responses
     are sent unless --prompts off.

` + "`terma install`" + ` then binds each repository to its project and wires the hooks that
stamp commits. Run setup again to change a choice; it rewrites the agents' settings
from the choices it recorded.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runSetup(cmd, f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.harnesses, "harness", "", "comma-separated agents to record ("+strings.Join(availableAgentNames(), ", ")+"); default: a picker")
	fl.BoolVar(&f.noBrowser, "no-browser", false, "print the sign-in URL instead of opening a browser")
	fl.BoolVarP(&f.assumeYes, "yes", "y", false, "skip the browser prompt and pickers; record every available installed agent and keep the recorded project")
	fl.StringVar(&f.projectRef, "project", "", "this machine's Terma project (name or id)")
	fl.BoolVar(&f.noRelay, "no-relay", false, "export straight to Terma, every session to this machine's project, instead of through terma's relay")
	fl.StringVar(&f.prompts, "prompts", "", "send prompt text and model responses: on or off (default: the recorded choice, on at first)")
	fl.StringVar(&f.signals, "signals", "", "comma-separated signals to export: traces, logs, metrics (default all)")
	fl.BoolVar(&f.excludeToolContent, "exclude-tool-content", false, "do not export tool parameters, input, or output")
	fl.StringVar(&f.identity, "identity", "", "identity stamped on Codex sessions (default: git user.email; \"none\" to omit)")
	fl.BoolVar(&f.noStatusLine, "no-statusline", false, "do not wrap Claude Code's status line (which captures the plan's rate-limit windows)")
	fl.BoolVar(&f.force, "force", false, "replace another collector's settings in an agent's global configuration")
	fl.BoolVarP(&f.verbose, "verbose", "v", false, "show what each step wrote")
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
	// The export choices, before anything is written: a bad flag must not leave a
	// signed-in machine with half a configuration.
	t := cfg.Telemetry
	if err := applySetupFlags(cmd, &t, f); err != nil {
		return err
	}

	// 1. Sign in — reusing the session this machine already has, verified.
	if cfg, err = signInAndReload(cmd, cfg, signInOptions{noBrowser: f.noBrowser, pauseBeforeBrowser: !f.assumeYes}); err != nil {
		return err
	}

	// 2. Which agents this developer uses.
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
	cfg.Harnesses = names

	ui := newInstallUI(out, f.verbose)
	ui.done, ui.loud = "terma set up", true
	if len(names) == 0 {
		ui.summary("Agents", "none recorded — `terma install` asks in each repository")
	} else {
		ui.summary("Agents", joinNames(adapterDisplayNames(names)))
	}

	// 3 and 4. The machine project, and each agent's global telemetry configuration.
	if len(machineAgents(names)) > 0 {
		project, err := chooseMachineProject(cmd, cfg, f.projectRef, !f.assumeYes && canPrompt(), config.ProjectRef{})
		if errors.Is(err, errCancelled) {
			fmt.Fprintln(out, "Cancelled. Agents were recorded; their telemetry was not configured.")
			return nil
		}
		if err != nil {
			return err
		}
		t.Project = project
		ui.summary("Project", nameOrID(project.Name, project.ID)+" — for sessions in a repository with no project of its own")
		sp := spinner.New(cmd.ErrOrStderr())
		sp.Start("Configuring your agents' telemetry…")
		res, err := configureMachineTelemetry(cmd.Context(), cmd.ErrOrStderr(), cfg, names, t, machineOptions{force: f.force, noStatusLine: f.noStatusLine})
		sp.Stop()
		if err != nil {
			return err
		}
		printMachineResult(ui, res)
		if res.telemetry.ExcludePrompts {
			ui.summary("Prompts", "prompt text and model responses are not sent — `terma setup --prompts on` sends them")
		} else {
			ui.summary("Prompts", "prompt text and model responses are sent — `terma setup --prompts off` stops them")
		}
	}
	if removed, err := cleanupLegacyRouting(); err != nil {
		ui.warn("Cleanup", "could not remove the old per-repository routing ("+err.Error()+")")
		ui.then("Run `terma shim uninstall` to remove it.")
	} else if removed {
		ui.summary("Cleanup", "removed the old per-repository routing (PATH shims, routing records)")
	}
	if slices.Contains(names, codexDesktopAgent) {
		ui.then("Codex Desktop: after `terma install` in a repository, open Settings → Hooks → Review in Codex Desktop and approve Terma's hooks.")
	}
	ui.then("Run `terma install` in each codebase you want to instrument with terma.")
	ui.finish()
	return nil
}

// applySetupFlags folds setup's export flags into the machine's recorded choices.
func applySetupFlags(cmd *cobra.Command, t *config.Telemetry, f setupFlags) error {
	flags := cmd.Flags()
	switch p := strings.ToLower(strings.TrimSpace(f.prompts)); p {
	case "on":
		t.ExcludePrompts = false
	case "off":
		t.ExcludePrompts = true
	case "":
	default:
		return fmt.Errorf("--prompts %q: want on or off", f.prompts)
	}
	if flags.Changed("signals") {
		if _, err := harness.ParseSignals(f.signals); err != nil {
			return err
		}
		t.Signals = f.signals
	}
	if flags.Changed("exclude-tool-content") {
		t.ExcludeToolContent = f.excludeToolContent
	}
	if flags.Changed("identity") {
		t.Identity = f.identity
	}
	if flags.Changed("no-relay") {
		t.NoRelay = f.noRelay
	}
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
	return name == "claude" || name == "codex" || name == codexDesktopAgent
}

func availableAgentNames() []string {
	var names []string
	for _, a := range harnessSelectionAgents() {
		if agentAvailable(a.Name()) {
			names = append(names, a.Name())
		}
	}
	return names
}

// harnessSelectionAgents puts available agents first, preserving registry order
// within each group. Both the form and its result mapping must use this order.
func harnessSelectionAgents() []agentChoice {
	var agents []agentChoice
	for _, available := range []bool{true, false} {
		for _, a := range adapter.All() {
			if agentAvailable(a.Name()) == available {
				adapterAgent := a
				display := a.DisplayName()
				if a.Name() == "codex" {
					display = "Codex CLI"
				}
				agents = append(agents, agentChoice{name: a.Name(), display: display, installed: adapterAgent.Installed})
				if a.Name() == "codex" {
					agents = append(agents, agentChoice{name: codexDesktopAgent, display: "Codex Desktop", installed: codexDesktopInstalled})
				}
			}
		}
	}
	return agents
}

func codexDesktopInstalled(context.Context) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	home, _ := os.UserHomeDir()
	for _, path := range []string{"/Applications/ChatGPT.app", filepath.Join(home, "Applications", "ChatGPT.app")} {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return true
		}
	}
	return false
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
		if n != codexDesktopAgent {
			if _, ok := adapter.Lookup(n); !ok {
				return nil, fmt.Errorf("unknown agent %q (want %s)", n, joinNames(availableAgentNames()))
			}
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
	for _, a := range harnessSelectionAgents() {
		if agentAvailable(a.Name()) && chosen[a.Name()] {
			names = append(names, a.Name())
		}
	}
	return names
}

// detectedAgents lists the supported agents whose binary is present on this machine.
func detectedAgents(ctx context.Context) []string {
	var names []string
	for _, a := range harnessSelectionAgents() {
		if agentAvailable(a.Name()) && a.Installed(ctx) {
			names = append(names, a.Name())
		}
	}
	return names
}

func agentDetail(ctx context.Context, name string) string {
	if name == codexDesktopAgent && codexDesktopInstalled(ctx) {
		return "installed"
	}
	if a, ok := adapter.Lookup(name); ok && a.Installed(ctx) {
		return "installed"
	}
	return ""
}

// adapterDisplayNames maps adapter tokens to their display names, in the given order.
func adapterDisplayNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == codexDesktopAgent {
			out = append(out, "Codex Desktop")
			continue
		}
		if n == "codex" {
			out = append(out, "Codex CLI")
			continue
		}
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
