package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

func (app *App) newTelemetryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "telemetry",
		Hidden:  true,
		Aliases: []string{"otel"},
		Short:   "Connect agent harnesses to Terma telemetry",
		Long: `Configures an agent CLI to export OpenTelemetry to Terma.

Connecting mints a server key scoped to one project and writes it, along with the
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
		Long: `Mints a server key for the selected project and writes the harness's telemetry
configuration.

The key is created server-side and returned exactly once. Where the harness can fetch
its OTLP headers from a script at startup, the key lands in a 0700 helper script under
~/.config/terma/helpers/ and the harness config gets only the script's path — so the
settings file never holds a credential and stays safe to share or keep in dotfiles;
pass --inline-key to write the key into the settings file instead. A harness with no
such mechanism always gets the key in its config. Either way, a file that holds the
key is tightened to 0600.

Your existing settings are preserved — only Terma's own keys are written, and
` + "`terma disconnect`" + ` removes exactly those. Reconnecting to the same project
reuses the key already installed rather than minting another; --api-key installs a
key you already hold.

Several harnesses can be named at once. Each is connected in turn and each holds a
key of its own, so one agent's key can be revoked without touching the other's.

On a terminal, a checklist first asks what to send and where; every box has a flag,
and --yes takes the flags and defaults without asking. --scope local writes the
repository you are in rather than your user settings: its committed settings get the
signal and content switches — nothing else, so it is safe to commit — and the agent
applies them over your global connect inside that repository. It needs no project,
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
			if _, ok := h.(harness.Scoped); !ok {
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
	// `--exports repos` switches nothing on globally, but still writes what a committed
	// policy cannot hold: endpoint, key, identity and the master switch.
	if reach == harness.ReachRepos {
		signals = nil
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
		return errors.New("telemetry connect needs a project — run `terma install` in this repository or pass --project")
	}

	// ConfigPath first: if the file cannot be located, a key minted below would be stranded.
	configPath, err := h.ConfigPath()
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	detection := h.Detect(ctx)

	// Built before the key exists so the conflict check runs first: a per-signal endpoint
	// left in place would inherit the Authorization header and hand out a live server key.
	intended := harness.Exporter{
		Endpoint:           cfg.OTLPURL,
		ProjectID:          cfg.ProjectID,
		Signals:            signals,
		ResourceAttributes: resourceAttributes(ctx, h, cfg, f.identity),
		IncludePrompts:     !f.excludePrompts,
		IncludeToolContent: !f.excludeToolContent,
	}
	// Where the harness supports it, a helper script supplies the header, so its settings
	// file holds only a path, never the key.
	if !f.inlineKey && h.SupportsHeadersHelper() {
		helperPath, err := harness.HelperFilePath(h, cfg.ProjectID)
		if err != nil {
			return err
		}
		intended.HelperPath = helperPath
	}
	conflicts, err := h.ConflictsWith(intended)
	if err != nil {
		return err
	}

	printConnectPlan(out, h, cfg, detection, configPath, intended.HelperPath, signals, reach, f)
	printConnectNotes(out, h, intended)
	printConflicts(out, conflicts, f.force)

	// Advisory conflicts (overrides that apply only under a selected profile) do not gate.
	conflicts, _ = partitionConflicts(conflicts)

	// A conflict outside the user file (the shell, a managed file, the user's own opt-out)
	// cannot be cleared by --force: the key would be installed while the override decides.
	if blocking := unclearable(conflicts); len(blocking) > 0 {
		return fmt.Errorf(
			"%s has settings Terma does not change: %s — remove or adjust them, then retry",
			h.DisplayName(), output.SanitizeTerminal(strings.Join(blocking, ", ")))
	}
	if len(conflicts) > 0 && !f.force {
		return fmt.Errorf(
			"%s already has OTLP settings that would override this connect — remove them, or pass --force to have Terma remove them",
			h.DisplayName())
	}

	if !f.assumeYes {
		ok, err := confirm(cmd, fmt.Sprintf("Connect %s to Terma?", h.DisplayName()))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, "Cancelled. Nothing was written.")
			return nil
		}
	}

	key, keyMeta, minted, reused, err := app.resolveKey(ctx, cfg, h, f)
	if err != nil {
		return err
	}
	if reused {
		fmt.Fprintf(out, "\nReusing the key already configured for this project (%s) — nothing new minted.\n", keyMeta.KeyPrefix)
	}

	// The backup is best-effort: a failure is reported, never blocking.
	if backup, err := backupHarnessConfig(h, cfg.OTLPURL); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not back up %s (%v).\n", configPath, err)
	} else if backup != "" {
		fmt.Fprintf(out, "\nBacked up %s\n", backup)
	}

	intended.APIKey = key
	if err := h.Connect(intended, f.force); err != nil {
		// Only a key this invocation minted; an --api-key key is still good.
		if minted {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"\nA key (%s) was minted before this failed. Revoke it in the web app if you do not retry.\n",
				keyMeta.KeyPrefix)
		}
		return err
	}
	// Kept per harness and project so the spool can deliver and `terma install` reuses it.
	if err := keystore.SetFor(h.Name(), cfg.ProjectID, key, keystore.HostsOf(cfg)); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not store the project key for the spool (%v); `terma spool flush` will not deliver until it is stored.\n", err)
	}
	statusLineNote := ""
	notifierNote := ""
	if line, ok := app.agents.Find[agents.StatusLiner](h.Name()); ok && !f.noStatusLine {
		statusLineNote, _ = installHarnessStatusLine(line, cmd.ErrOrStderr())
	}
	if notifier, ok := app.agents.Find[agents.Notifier](h.Name()); ok {
		switch changed, err := notifier.InstallNotifier(); {
		case err != nil:
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not install %s's funding notifier (%v).\n", h.DisplayName(), err)
		case changed:
			notifierNote = "Notifier: terma will capture plan and quota at the end of each turn; any previous notifier keeps running behind it."
		default:
			notifierNote = "Notifier: terma's plan and quota capture is already installed."
		}
	}
	fmt.Fprintf(out, "\nConnected. Restart %s, then run a prompt.\n", h.DisplayName())
	if statusLineNote != "" {
		fmt.Fprintln(out, statusLineNote)
	}
	if notifierNote != "" {
		fmt.Fprintln(out, notifierNote)
	}
	fmt.Fprintln(out, "See its sessions with `terma session list`.")
	return nil
}

// printConnectPlan always prints the redaction lines: "off" is the answer a reader wants.
func printConnectPlan(
	out io.Writer,
	h harness.Harness,
	cfg *config.Config,
	detection harness.Detection,
	configPath, helperPath string,
	signals []harness.Signal,
	reach harness.Reach,
	f connectFlags,
) {
	printDetection(out, h, detection)
	fmt.Fprintf(out, "  Terma project: %s\n", cmp.Or(cfg.ProjectName, cfg.ProjectID))
	fmt.Fprintf(out, "  Endpoint:        %s\n", cfg.OTLPURL)

	if reach == harness.ReachRepos {
		// Three empty checkboxes would read as a mistake rather than as the arrangement asked for.
		fmt.Fprintf(out, "  Exports from:    repositories that carry a terma policy — every exporter here is left off\n")
		fmt.Fprintln(out, "\n  Signals:")
		fmt.Fprintln(out, "    decided by each repository's committed terma policy")
		fmt.Fprintf(out, "\n    Prompts:      %s (a repository may narrow this, never widen the machine's reach)\n", onOff(!f.excludePrompts))
		fmt.Fprintf(out, "    Tool content: %s\n", onOff(!f.excludeToolContent))
	} else {
		printDataPlan(out, signals, !f.excludePrompts, !f.excludeToolContent)
	}

	fmt.Fprintln(out, "\n  This will update:")
	fmt.Fprintf(out, "    %s\n", configPath)
	if helperPath != "" {
		fmt.Fprintf(out, "    %s  (holds the key; the settings file will not)\n", helperPath)
	} else {
		fmt.Fprintln(out, "    (the server key is written into this file, which is tightened to 0600)")
	}
	if f.apiKey == "" {
		// Reuse is decided after the plan, so the plan states the rule.
		fmt.Fprintln(out, "\n  A server key will be minted for this project — unless one is already installed here, which will be reused.")
	} else {
		fmt.Fprintf(out, "\n  Installing the key you supplied (%s).\n", harness.MaskKey(f.apiKey))
	}
	fmt.Fprintln(out)
}

func printConnectNotes(out io.Writer, h harness.Harness, e harness.Exporter) {
	n, ok := h.(harness.Noter)
	if !ok {
		return
	}
	notes := n.ConnectNotes(e)
	if len(notes) == 0 {
		return
	}
	fmt.Fprintln(out, "  Note:")
	for _, note := range notes {
		fmt.Fprintf(out, "    %s\n", output.SanitizeTerminal(note))
	}
	fmt.Fprintln(out)
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
	if cur, ok := h.(harness.Credentialed); ok {
		if existing, ok := cur.CurrentCredential(cfg.OTLPURL, cfg.ProjectID); ok {
			return existing, api.ServerKey{KeyPrefix: harness.MaskKey(existing)}, false, true, nil
		}
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

// resourceAttributes takes identity "none" to omit enduser.id, whose default is a real
// email written into a global config file.
func resourceAttributes(ctx context.Context, h harness.Harness, cfg *config.Config, identity string) map[string]string {
	attrs := map[string]string{
		harness.AttrServiceName: harness.ServiceName(h),
		harness.AttrProjectID:   cfg.ProjectID,
	}

	switch identity = strings.TrimSpace(identity); identity {
	case "none":
	case "":
		// git's global email: a repository-local one would follow the user out of its repo.
		if email := harness.GitEmail(ctx); email != "" {
			attrs[harness.AttrEnduserID] = email
		}
	default:
		attrs[harness.AttrEnduserID] = identity
	}
	return attrs
}

// printConflicts never prints a header value (it may hold someone else's credential) and
// sanitizes every field, since a key can carry a file name.
func printConflicts(out io.Writer, conflicts []harness.Conflict, force bool) {
	blocking, advisory := partitionConflicts(conflicts)

	if len(blocking) > 0 {
		if force {
			fmt.Fprintln(out, "  Conflicting settings, which --force will remove where it can:")
		} else {
			fmt.Fprintln(out, "  Conflicting settings:")
		}
		for _, c := range blocking {
			printConflict(out, c)
		}
		fmt.Fprintln(out)
	}
	if len(advisory) > 0 {
		fmt.Fprintln(out, "  Overrides that apply only in a mode you select explicitly — reported, not blocking:")
		for _, c := range advisory {
			printConflict(out, c)
		}
		fmt.Fprintln(out)
	}
}

func printConflict(out io.Writer, c harness.Conflict) {
	key := output.SanitizeTerminal(c.Key)
	if c.Value != "" {
		fmt.Fprintf(out, "    %s=%s\n", key, output.SanitizeTerminal(c.Value))
	} else {
		fmt.Fprintf(out, "    %s\n", key)
	}
	marker := " "
	if c.Credential {
		marker = "!"
	}
	fmt.Fprintf(out, "     %s [%s] %s\n", marker, output.SanitizeTerminal(c.Scope), output.SanitizeTerminal(c.Reason))
	if !c.Clearable && !c.Advisory {
		// Said here and in the error: the user has to fix this one themselves.
		if c.Scope == harness.ScopeUserSettings {
			fmt.Fprintf(out, "       Terma does not change this — it is your setting to remove.\n")
		} else {
			fmt.Fprintf(out, "       Terma cannot change this — it is outside the file Terma writes.\n")
		}
	}
}

func partitionConflicts(conflicts []harness.Conflict) (blocking, advisory []harness.Conflict) {
	for _, c := range conflicts {
		if c.Advisory {
			advisory = append(advisory, c)
		} else {
			blocking = append(blocking, c)
		}
	}
	return blocking, advisory
}

func unclearable(conflicts []harness.Conflict) []string {
	var out []string
	for _, c := range conflicts {
		if !c.Clearable {
			out = append(out, c.Key)
		}
	}
	return out
}

func backupHarnessConfig(h harness.Harness, endpoint string) (string, error) {
	if b, ok := h.(harness.Backuper); ok {
		return b.Backup(endpoint)
	}
	return "", nil
}

// installStatusLine only warns on failure: the exporters are already written and working.
func (app *App) installStatusLine(errOut io.Writer) (string, bool) {
	s, ok := doctor.StatusLineAgent(app.agents)
	if !ok {
		return "", false
	}
	return installHarnessStatusLine(s, errOut)
}

func installHarnessStatusLine(c agents.StatusLiner, errOut io.Writer) (string, bool) {
	changed, err := c.InstallStatusLine()
	if err != nil {
		fmt.Fprintf(errOut, "Warning: could not wrap %s's status line (%v); plan usage will not be captured.\n", c.DisplayName(), err)
		return "", false
	}
	st, stErr := c.StatusLineState("")
	switch {
	case stErr != nil:
		return "", true
	case changed && st.Renderer != "":
		return fmt.Sprintf("Status line: terma now reads the plan's usage windows from it; your own status line (%s) keeps running unchanged behind it.", output.SanitizeTerminal(st.Renderer)), true
	case changed:
		return "Status line: terma added one that shows model, context, cost and the plan's usage windows (remove it with `terma disconnect " + c.Name() + "`, or skip it with --no-statusline).", true
	default:
		return "Status line: already wrapped by terma.", true
	}
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

func containsSignal(signals []harness.Signal, want harness.Signal) bool {
	return slices.Contains(signals, want)
}

func joinSignals(signals []harness.Signal) string {
	parts := make([]string, 0, len(signals))
	for _, s := range signals {
		parts = append(parts, string(s))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func signalLabel(s harness.Signal) string {
	switch s {
	case harness.SignalTraces:
		return "Agent traces"
	case harness.SignalLogs:
		return "Structured events"
	case harness.SignalMetrics:
		return "Token and cost metrics"
	default:
		return string(s)
	}
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}
