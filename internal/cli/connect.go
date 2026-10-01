package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/connect"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

func (app *App) newTelemetryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "telemetry",
		Hidden:  true,
		Aliases: []string{"otel"},
		Short:   "Connect agent harnesses to Terma telemetry",
		Long: `Configures an agent CLI to export OpenTelemetry to Terma.

Connecting mints a server key scoped to one team and writes it, along with the
OTLP endpoint, into the harness's own configuration. Nothing is added to your shell
profile, and no other setting in that file is touched.

Everything is captured by default — traces, events, metrics, prompt text, model
responses, and tool input/output. That content is what makes an agent trace worth
reading, but it does mean what you and the model said leaves this machine. On a
terminal, connect shows a checklist to untick what should stay; --exclude-prompts,
--exclude-tool-content and --signals decide the same thing without one.

A connect is global by default — this machine, every repository. --scope local
writes a repository's own policy into its committed settings instead: only what to
ship, never where or with which key, so one repository can send less than the machine
does (` + app.scopedHarnessNames() + `).

Supported: ` + strings.Join(app.agents.HarnessNames(), ", ") + `.`,
	}
	cmd.AddCommand(app.newTelemetryConnectCommand(), app.newTelemetryStatusCommand(), app.newTelemetryDisconnectCommand())
	return cmd
}

type connectFlags struct {
	scope   string
	signals string
	// exports is the Reach of a global connect; meaningless at local scope.
	exports string
	// The content switches are exclusions because capture is the default: a redacted
	// trace answers almost none of the questions that send someone to it.
	excludePrompts     bool
	excludeToolContent bool
	// noStatusLine skips the status-line wrap, whose payload carries the provider's own
	// rate-limit windows, the strongest funding evidence a machine produces.
	noStatusLine bool
	keyName      string
	apiKey       string
	identity     string
	inlineKey    bool
	assumeYes    bool
	force        bool
}

func (app *App) newTelemetryConnectCommand() *cobra.Command {
	var f connectFlags

	cmd := &cobra.Command{
		Use:    "connect <" + strings.Join(app.agents.HarnessNames(), "|") + "> [<harness>...]",
		Short:  "Point one or more agent harnesses at Terma",
		Hidden: true,
		Long: `Mints a server key for the selected team and writes the harness's telemetry
configuration.

The key is created server-side and returned exactly once. Where the harness can fetch
its OTLP headers from a script at startup, the key lands in a 0700 helper script under
~/.config/terma/helpers/ and the harness config gets only the script's path — so the
settings file never holds a credential and stays safe to share or keep in dotfiles;
pass --inline-key to write the key into the settings file instead. A harness with no
such mechanism always gets the key in its config. Either way, a file that holds the
key is tightened to 0600.

Your existing settings are preserved — only Terma's own keys are written, and
` + "`terma disconnect`" + ` removes exactly those. Reconnecting to the same team
reuses the key already installed rather than minting another; --api-key installs a
key you already hold.

Several harnesses can be named at once. Each is connected in turn and each holds a
key of its own, so one agent's key can be revoked without touching the other's.

On a terminal, a checklist first asks what to send and where; every box has a flag,
and --yes takes the flags and defaults without asking. --scope local writes the
repository you are in rather than your user settings: its committed settings get the
signal and content switches — nothing else, so it is safe to commit — and the agent
applies them over your global connect inside that repository. It needs no team,
key or sign-in (` + app.scopedHarnessNames() + `).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.runTelemetryConnectAll(cmd, args, f)
		},
	}

	fl := cmd.Flags()
	fl.StringVar(&f.scope, "scope", "", "where to write: global (this machine, default) or local (this repository's own settings; "+app.scopedHarnessNames()+")")
	fl.StringVar(&f.signals, "signals", "", "comma-separated signals to export: traces, logs, metrics (default all; none to ship nothing)")
	fl.StringVar(&f.exports, "exports", "", "which repositories export: everywhere (this machine, default) or repos (only those whose committed terma policy turns it on)")
	fl.BoolVar(&f.excludePrompts, "exclude-prompts", false, "do not export prompt text or model responses")
	fl.BoolVar(&f.excludeToolContent, "exclude-tool-content", false, "do not export tool parameters, input, or output")
	fl.BoolVar(&f.noStatusLine, "no-statusline", false, "leave "+app.statusLineOwner()+"'s status line alone (by default terma wraps it to read the plan's rate-limit windows; the configured command keeps running unchanged)")
	fl.StringVar(&f.keyName, "key-name", "", "name for the minted key (defaults to <harness>@<hostname>)")
	fl.StringVar(&f.apiKey, "api-key", "", "install this existing server key (ter_srv_…) instead of minting a new one")
	fl.StringVar(&f.identity, "identity", "", "value for enduser.id on the sessions of agents that take one (defaults to your global git email; \"none\" to omit); an agent that reports its signed-in account keeps that")
	fl.BoolVar(&f.inlineKey, "inline-key", false, "store the key in the settings file instead of a Terma headers-helper script, for agents that read one")
	fl.BoolVarP(&f.assumeYes, "yes", "y", false, "skip the confirmation prompt")
	fl.BoolVar(&f.force, "force", false, "remove conflicting per-signal OTLP settings instead of refusing to connect")
	return cmd
}

// runTelemetryConnectAll resolves every name up front so a typo in the last is refused
// before the first is touched.
func (app *App) runTelemetryConnectAll(cmd *cobra.Command, names []string, f connectFlags) error {
	var hs []harness.Harness
	seen := map[string]bool{}
	for _, name := range names {
		h, err := app.agents.Harness(name)
		if err != nil {
			return err
		}
		if seen[h.Name()] {
			continue
		}
		seen[h.Name()] = true
		hs = append(hs, h)
	}
	out := cmd.OutOrStdout()

	// One checklist for all: what to send is one decision, and an unscoped harness is
	// refused before the first one is written.
	if !f.assumeYes && canPrompt() {
		root, _ := localRoot(cmd.Context())
		picked, err := askConnectOptions(f, hs, connectForm{root: root})
		if errors.Is(err, errCancelled) {
			fmt.Fprintln(out, "Cancelled. Nothing was written.")
			return nil
		}
		if err != nil {
			return err
		}
		f = picked
	}
	scope, err := harness.ParseScope(f.scope)
	if err != nil {
		return err
	}
	if scope == harness.ScopeLocal {
		for _, h := range hs {
			if _, ok := h.Local(""); !ok {
				return fmt.Errorf("%s has no repository settings — --scope local applies to harnesses that read one (%s)", h.DisplayName(), app.scopedHarnessNames())
			}
		}
	}

	for i, h := range hs {
		if len(hs) > 1 {
			if i > 0 {
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "— %s —\n", h.Name())
		}
		if err := app.runTelemetryConnect(cmd, h.Name(), f); err != nil {
			if len(hs) > 1 {
				return fmt.Errorf("connect %s: %w", h.Name(), err)
			}
			return err
		}
	}
	return nil
}

func (app *App) runTelemetryConnect(cmd *cobra.Command, name string, f connectFlags) error {
	scope, err := harness.ParseScope(f.scope)
	if err != nil {
		return err
	}
	if scope == harness.ScopeLocal {
		return app.runLocalConnect(cmd, name, f)
	}
	err = app.connectGlobal(cmd, name, f)
	return err
}

func (app *App) connectGlobal(cmd *cobra.Command, name string, f connectFlags) error {
	h, err := app.agents.Harness(name)
	if err != nil {
		return err
	}
	signals, err := harness.ParseSignals(f.signals)
	if err != nil {
		return err
	}
	reach, err := harness.ParseReach(f.exports)
	if err != nil {
		return err
	}
	cfg, err := app.loadConfig()
	if err != nil {
		return err
	}
	if err := requireProject(cfg); err != nil {
		return err
	}
	// A server key is bound to a project id; under TERMA_API_KEY the key's own grant fixes it.
	if cfg.ProjectID == "" {
		return errors.New("telemetry connect needs a team — run `terma install` in this repository or pass --team")
	}
	ctx := cmd.Context()
	return connect.Global(ctx, app.agents, h, cfg, f.options(signals, reach), connect.Steps{
		Confirm: func(q string) (bool, error) { return confirm(cmd, q) },
		Key: func(ctx context.Context) (connect.Key, error) {
			key, meta, minted, reused, err := app.resolveKey(ctx, cfg, h, f)
			return connect.Key{Value: key, Prefix: meta.KeyPrefix, Minted: minted, Reused: reused}, err
		},
		Store: func(key string) error { return keystore.SetFor(h.Name(), cfg.ProjectID, key, keystore.HostsOf(cfg)) },
	}, connect.IO{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()})
}

// options are the connect choices the flags make.
func (f connectFlags) options(signals []harness.Signal, reach harness.Reach) connect.Options {
	return connect.Options{Signals: signals, Reach: reach, Identity: f.identity, SuppliedKey: strings.TrimSpace(f.apiKey),
		ExcludePrompts: f.excludePrompts, ExcludeToolContent: f.excludeToolContent,
		InlineKey: f.inlineKey, Force: f.force, AssumeYes: f.assumeYes, NoStatusLine: f.noStatusLine}
}

// resolveKey reports minted because only a key this invocation created is the caller's
// to clean up if a later step fails.
func (app *App) resolveKey(ctx context.Context, cfg *config.Config, h harness.Harness, f connectFlags) (key string, meta api.ServerKey, minted, reused bool, err error) {
	if key := strings.TrimSpace(f.apiKey); key != "" {
		if !serverkey.Is(key) {
			return "", api.ServerKey{}, false, false, errors.New("--api-key expects a server key (ter_srv_…)")
		}
		return key, api.ServerKey{KeyPrefix: harness.MaskKey(key)}, false, false, nil
	}

	// Reuse the key for this exact endpoint and project, per harness: minting on every
	// tweak leaves live orphaned keys, and one agent's key can be revoked alone.
	if existing, ok := h.CurrentCredential(cfg.OTLPURL, cfg.ProjectID); ok {
		return existing, api.ServerKey{KeyPrefix: harness.MaskKey(existing)}, false, true, nil
	}
	// Pointed elsewhere now, it may have been connected to this project before.
	if remembered := keystore.GetFor(h.Name(), cfg.ProjectID); remembered != "" {
		return remembered, api.ServerKey{KeyPrefix: harness.MaskKey(remembered)}, false, true, nil
	}

	name := strings.TrimSpace(f.keyName)
	if name == "" {
		name = h.Name() + "@" + hostname()
	}

	client, err := app.newClient(cfg)
	if err != nil {
		return "", api.ServerKey{}, false, false, err
	}
	key, meta, err = client.CreateServerKey(ctx, cfg.ProjectID, name,
		"Created by terma connect "+h.Name())
	if err != nil {
		return "", api.ServerKey{}, false, false, err
	}
	return key, meta, true, false, nil
}

// installStatusLine only warns on failure: the exporters are already written and working.
func (app *App) installStatusLine(errOut io.Writer) (string, bool) {
	s, ok := doctor.StatusLineAgent(app.agents)
	if !ok {
		return "", false
	}
	return connect.InstallStatusLine(s, errOut)
}

func confirm(cmd *cobra.Command, question string) (bool, error) {
	return confirmDefault(cmd, question, true)
}

func confirmDefault(cmd *cobra.Command, question string, def bool) (bool, error) {
	return confirmExplained(cmd, question, nil, def)
}

func confirmExplained(cmd *cobra.Command, question string, detail []string, def bool) (bool, error) {
	in := cmd.InOrStdin()
	interactive := false
	// /dev/null (including go test's stdin) is not an explicit piped answer.
	if f, ok := in.(*os.File); ok && !term.IsTerminal(int(f.Fd())) {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return false, fmt.Errorf("%s — no input to confirm with; pass --yes or pipe an answer", question)
		}
	}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		if !canPrompt() {
			return false, fmt.Errorf("%s — no terminal to confirm on; pass --yes to proceed non-interactively", question)
		}
		state, err := term.MakeRaw(int(f.Fd()))
		if err != nil {
			return false, fmt.Errorf("read confirmation: %w", err)
		}
		defer func() { _ = term.Restore(int(f.Fd()), state) }()
		interactive = true
	}
	errOut := cmd.ErrOrStderr()
	text := confirmPrompt(style.For(errOut), question, detail, def)
	if interactive {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	fmt.Fprint(errOut, text)
	answer, err := readConfirmation(in, interactive, def)
	if interactive {
		if err == nil {
			if answer {
				fmt.Fprint(errOut, "y")
			} else {
				fmt.Fprint(errOut, "n")
			}
		}
		fmt.Fprint(errOut, "\r\n")
	}
	return answer, err
}

func confirmPrompt(p style.Palette, question string, detail []string, def bool) string {
	hint := "[Y/n]"
	if !def {
		hint = "[y/N]"
	}
	if len(detail) == 0 {
		return fmt.Sprintf("%s %s %s ", p.Brand("?"), p.Bold(question), p.Dim(hint))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", p.Brand("?"), p.Bold(question))
	for _, l := range detail {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	fmt.Fprintf(&b, "  %s ", p.Dim(hint))
	return b.String()
}

// yesAnswer treats anything but y, yes or nothing as no, so a typo never agrees to something.
func yesAnswer(line string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "":
		return def
	}
	return false
}
