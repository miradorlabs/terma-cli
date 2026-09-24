package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/api"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/output"
	"github.com/miradorlabs/terma-cli/internal/serverkey"
	"github.com/miradorlabs/terma-cli/internal/style"
)

func newTelemetryCommand() *cobra.Command {
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
writes a repository's own policy into its committed .claude/settings.json instead:
only what to ship, never where or with which key, so one repository can send less
than the machine does. Claude Code and OpenCode; Codex reads a single config file.

Supported: ` + strings.Join(harness.Names(), ", ") + `.`,
	}
	cmd.AddCommand(newTelemetryConnectCommand(), newTelemetryStatusCommand(), newTelemetryDisconnectCommand())
	return cmd
}

type connectFlags struct {
	// scope is global (the harness's user settings) or local (this repository's
	// project settings); see harness.Scope.
	scope   string
	signals string
	// exports is the Reach of a global connect: everywhere (default) or repos, which
	// writes the destination and the key but leaves every exporter off so that each
	// repository's own committed policy decides. Meaningless at local scope, where
	// deciding what to ship is the whole point of the file.
	exports string
	// The content switches are spelled as exclusions because capture is the default:
	// the point of connecting an agent harness is to see what the agent did, and a
	// trace with the prompt and tool activity redacted answers almost none of the
	// questions that send someone to it. The flags exist for the environments where
	// that content must not leave the machine.
	excludePrompts     bool
	excludeToolContent bool
	// noStatusLine leaves Claude Code's statusLine alone. By default a global
	// connect puts `terma hook statusline` in front of it: the status line payload
	// carries the provider's own rate-limit windows, the strongest funding evidence
	// a machine produces, and the previous command keeps running unchanged behind it.
	noStatusLine bool
	keyName      string
	apiKey       string
	identity     string
	inlineKey    bool
	assumeYes    bool
	force        bool
}

func newTelemetryConnectCommand() *cobra.Command {
	var f connectFlags

	cmd := &cobra.Command{
		Use:    "connect <" + strings.Join(harness.Names(), "|") + "> [<harness>...]",
		Short:  "Point one or more agent harnesses at Terma",
		Hidden: true,
		Long: `Mints a server key for the selected project and writes the harness's telemetry
configuration.

The key is created server-side and returned exactly once. Where the harness can fetch
its OTLP headers from a script at startup (Claude Code's otelHeadersHelper), the key
lands in a 0700 helper script under ~/.config/terma/helpers/ and the harness config gets
only the script's path — so the settings file never holds a credential and stays safe
to share or keep in dotfiles; pass --inline-key to write the key into the settings
file instead. Codex has no such mechanism, so its key is always written into
config.toml. Either way, a file that holds the key is tightened to 0600.

Your existing settings are preserved — only Terma's own keys are written, and
` + "`terma disconnect`" + ` removes exactly those. Reconnecting to the same project
reuses the key already installed rather than minting another; --api-key installs a
key you already hold.

Several harnesses can be named at once. Each is connected in turn and each holds a
key of its own, so one agent's key can be revoked without touching the other's.

On a terminal, a checklist first asks what to send and where; every box has a flag,
and --yes takes the flags and defaults without asking. --scope local writes the
repository you are in rather than your user settings: its .claude/settings.json gets
the signal and content switches — nothing else, so it is safe to commit — and Claude
Code applies them over your global connect inside that repository. It needs no
project, key or sign-in. Claude Code and OpenCode; Codex reads a single config file.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTelemetryConnectAll(cmd, args, f)
		},
	}

	fl := cmd.Flags()
	fl.StringVar(&f.scope, "scope", "", "where to write: global (this machine, default) or local (this repository's own settings; Claude Code and OpenCode)")
	fl.StringVar(&f.signals, "signals", "", "comma-separated signals to export: traces, logs, metrics (default all; none to ship nothing)")
	fl.StringVar(&f.exports, "exports", "", "which repositories export: everywhere (this machine, default) or repos (only those whose committed terma policy turns it on)")
	fl.BoolVar(&f.excludePrompts, "exclude-prompts", false, "do not export prompt text or model responses")
	fl.BoolVar(&f.excludeToolContent, "exclude-tool-content", false, "do not export tool parameters, input, or output")
	fl.BoolVar(&f.noStatusLine, "no-statusline", false, "leave Claude Code's status line alone (by default terma wraps it to read the plan's rate-limit windows; the configured command keeps running unchanged)")
	fl.StringVar(&f.keyName, "key-name", "", "name for the minted key (defaults to <harness>@<hostname>)")
	fl.StringVar(&f.apiKey, "api-key", "", "install this existing server key (ter_srv_…) instead of minting a new one")
	fl.StringVar(&f.identity, "identity", "", "value for enduser.id on Codex and OpenCode sessions (defaults to your global git email; \"none\" to omit). Claude Code reports the account it is signed in with")
	fl.BoolVar(&f.inlineKey, "inline-key", false, "store the key in the settings file instead of a Terma headers-helper script (Claude Code; Codex always stores it inline)")
	fl.BoolVarP(&f.assumeYes, "yes", "y", false, "skip the confirmation prompt")
	fl.BoolVar(&f.force, "force", false, "remove conflicting per-signal OTLP settings instead of refusing to connect")
	return cmd
}

// runTelemetryConnectAll connects each named harness in turn, each with its own key.
// Names are resolved up front so a typo in the last one is refused before the first
// is touched.
func runTelemetryConnectAll(cmd *cobra.Command, names []string, f connectFlags) error {
	var hs []harness.Harness
	seen := map[string]bool{}
	for _, name := range names {
		h, err := harness.Lookup(name)
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

	// The checklist runs once for all of them: what to send is one decision, and a
	// harness without repository settings is refused up front rather than after the
	// first one has been written.
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
				return fmt.Errorf("%s has no repository settings — --scope local applies to harnesses that read one (Claude Code, OpenCode)", h.DisplayName())
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
		if err := runTelemetryConnect(cmd, h.Name(), f); err != nil {
			if len(hs) > 1 {
				return fmt.Errorf("connect %s: %w", h.Name(), err)
			}
			return err
		}
	}
	return nil
}

func runTelemetryConnect(cmd *cobra.Command, name string, f connectFlags) error {
	scope, err := harness.ParseScope(f.scope)
	if err != nil {
		return err
	}
	if scope == harness.ScopeLocal {
		return runLocalConnect(cmd, name, f)
	}
	err = connectGlobal(cmd, name, f)
	return err
}

// connectGlobal writes a harness's user-level telemetry settings and reports what it
// did about the key.
func connectGlobal(cmd *cobra.Command, name string, f connectFlags) error {
	h, err := harness.Lookup(name)
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
	// `--exports repos` hands the decision to each repository, so the global file must
	// switch nothing on. It still carries the endpoint, the key, the identity and the
	// master telemetry switch — everything a repository's committed policy cannot hold
	// and needs underneath it.
	if reach == harness.ReachRepos {
		signals = nil
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := requireProject(cfg); err != nil {
		return err
	}
	// A server key is bound to a project id, so the id has to be known locally. Under
	// TERMA_API_KEY the project is fixed by the key's own grant and never recorded in
	// the repository or an override, which is different from an implicit server-key scope.
	if cfg.ProjectID == "" {
		return errors.New("telemetry connect needs a project — run `terma install` in this repository or pass --project")
	}

	// ConfigPath before anything else: if the file cannot even be located, nothing below
	// is worth doing, and a key minted here would be stranded.
	configPath, err := h.ConfigPath()
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	detection := h.Detect(ctx)

	// The exporter this connect intends to install. Built before the key exists so the
	// conflict check can run first: a per-signal endpoint or a beta-tracing redirect left
	// in place would keep exporting to whoever owns it while inheriting the Authorization
	// header Terma is about to write — handing out a live server key. Once the key is
	// on disk that is invisible, so it has to be caught here.
	intended := harness.Exporter{
		Endpoint:           cfg.OTLPURL,
		ProjectID:          cfg.ProjectID,
		Signals:            signals,
		ResourceAttributes: resourceAttributes(ctx, h, cfg, f.identity),
		IncludePrompts:     !f.excludePrompts,
		IncludeToolContent: !f.excludeToolContent,
	}
	// The default delivery, where the harness supports it: a Terma-owned helper script
	// supplies the Authorization header, so the harness's settings file never holds the
	// key — only a path. A harness without the mechanism gets the key inline.
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

	// Advisory conflicts — a Codex profile's overrides, which apply only when that
	// profile is selected — are shown above and do not gate the connect.
	conflicts, _ = partitionConflicts(conflicts)

	// A conflict Terma does not change — exported in the shell, set in a managed file
	// that outranks the user config, or a setting that is the user's own opt-out —
	// cannot be cleared by writing to the user file, so --force must not pretend
	// otherwise. Connecting anyway would install the credential while the override
	// kept deciding where telemetry goes.
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

	key, keyMeta, minted, reused, err := resolveKey(ctx, cfg, h, f)
	if err != nil {
		return err
	}
	if reused {
		fmt.Fprintf(out, "\nReusing the key already configured for this project (%s) — nothing new minted.\n", keyMeta.KeyPrefix)
	}

	// Back up before the merge. Best-effort: a user who asked to connect should not be
	// blocked because a backup could not be written, but they should hear about it.
	if backup, err := backupHarnessConfig(h, cfg.OTLPURL); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not back up %s (%v).\n", configPath, err)
	} else if backup != "" {
		fmt.Fprintf(out, "\nBacked up %s\n", backup)
	}

	intended.APIKey = key
	if err := h.Connect(intended, f.force); err != nil {
		// Only when this invocation created it. A key supplied with --api-key already
		// existed and is still perfectly good, so telling the user to go revoke it would
		// send them to destroy a working credential over an unrelated write failure.
		if minted {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"\nA key (%s) was minted before this failed. Revoke it in the web app if you do not retry.\n",
				keyMeta.KeyPrefix)
		}
		return err
	}
	// Remember the key per harness and per project, so the spool can deliver this
	// project's events and a later `terma install` for this project reuses the key
	// without minting again.
	if err := keystore.SetFor(h.Name(), cfg.ProjectID, key); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not store the project key for the spool (%v); `terma spool flush` will not deliver until it is stored.\n", err)
	}
	statusLineNote := ""
	codexNotifyNote := ""
	if h.Name() == "claude" && !f.noStatusLine {
		statusLineNote = installStatusLine(cmd.ErrOrStderr())
	}
	if h.Name() == "codex" {
		switch changed, err := (harness.Codex{}).InstallCodexNotify(); {
		case err != nil:
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not install Codex's funding notifier (%v).\n", err)
		case changed:
			codexNotifyNote = "Notifier: terma will capture plan and quota at the end of each turn; any previous notifier keeps running behind it."
		default:
			codexNotifyNote = "Notifier: terma's plan and quota capture is already installed."
		}
	}
	fmt.Fprintf(out, "\nConnected. Restart %s, then run a prompt.\n", h.DisplayName())
	if statusLineNote != "" {
		fmt.Fprintln(out, statusLineNote)
	}
	if codexNotifyNote != "" {
		fmt.Fprintln(out, codexNotifyNote)
	}
	fmt.Fprintln(out, "See its sessions with `terma session list`.")
	return nil
}

// printConnectPlan shows exactly what a connect would do, before it does any of it.
// The redaction lines are always printed, including when they are off — "off" is the
// answer to the question a reader actually has.
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
	fmt.Fprintf(out, "  Terma project: %s\n", nameOrID(cfg.ProjectName, cfg.ProjectID))
	fmt.Fprintf(out, "  Endpoint:        %s\n", cfg.OTLPURL)

	if reach == harness.ReachRepos {
		// Without this the plan shows three empty checkboxes, which reads as a mistake
		// rather than as the arrangement the user asked for.
		fmt.Fprintf(out, "  Exports from:    repositories that carry a terma policy — every exporter here is left off\n")
		fmt.Fprintln(out, "\n  Signals:")
		fmt.Fprintln(out, "    decided by each repository's committed .claude/settings.json")
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
		// Reuse is decided after the plan (it needs the config read that minting also
		// waits on), so the plan states the rule rather than predicting the branch.
		fmt.Fprintln(out, "\n  A server key will be minted for this project — unless one is already installed here, which will be reused.")
	} else {
		fmt.Fprintf(out, "\n  Installing the key you supplied (%s).\n", harness.MaskKey(f.apiKey))
	}
	fmt.Fprintln(out)
}

// printConnectNotes prints what a harness wants said about this particular connect —
// a side effect of its own, or a limit of what its switches can do — before the user
// is asked to confirm. Optional: most connects have nothing to add.
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

// resolveKey either installs a key the caller already holds or mints a new one. The
// bool reports which happened, because only a key this invocation created is the
// caller's to clean up if a later step fails.
func resolveKey(ctx context.Context, cfg *config.Config, h harness.Harness, f connectFlags) (key string, meta api.ServerKey, minted, reused bool, err error) {
	if key := strings.TrimSpace(f.apiKey); key != "" {
		if !serverkey.Is(key) {
			return "", api.ServerKey{}, false, false, errors.New("--api-key expects a server key (ter_srv_…)")
		}
		return key, api.ServerKey{KeyPrefix: harness.MaskKey(key)}, false, false, nil
	}

	// Reconnects reuse the key already installed for this exact endpoint and project.
	// Minting on every settings tweak — flipping a capture flag, changing signals —
	// would leave a trail of live orphaned keys that nobody remembers and nothing
	// cleans up; the key already here is exactly as scoped as the one a mint would
	// produce. A different project or endpoint falls through to a fresh mint, because
	// reusing across either boundary would be wrong, not just untidy. Reuse is per
	// harness on purpose: each agent holds its own key, so one can be revoked without
	// cutting the other off.
	if cur, ok := h.(harness.Credentialed); ok {
		if existing, ok := cur.CurrentCredential(cfg.OTLPURL, cfg.ProjectID); ok {
			return existing, api.ServerKey{KeyPrefix: harness.MaskKey(existing)}, false, true, nil
		}
	}
	// The harness is pointed elsewhere now, but this machine may have connected it to
	// this project before — moving between repositories does exactly that. The key it
	// used then is still its own and still scoped to this project.
	if remembered := keystore.GetFor(h.Name(), cfg.ProjectID); remembered != "" {
		return remembered, api.ServerKey{KeyPrefix: harness.MaskKey(remembered)}, false, true, nil
	}

	name := strings.TrimSpace(f.keyName)
	if name == "" {
		name = h.Name() + "@" + harness.Hostname()
	}

	client, err := newClient(cfg)
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

// resourceAttributes are stamped on everything the harness emits — Codex and OpenCode,
// that is: the Claude Code adapter renders none of them, because OTEL_RESOURCE_ATTRIBUTES
// is the user's variable and Claude Code carries its own identity and service name.
//
// identity overrides the default enduser.id. The literal "none" omits it — worth having
// because the default is a real email address written into a global config file, and
// there is no other way to say "do not label my sessions".
func resourceAttributes(ctx context.Context, h harness.Harness, cfg *config.Config, identity string) map[string]string {
	attrs := map[string]string{
		harness.AttrServiceName: harness.ServiceName(h),
		harness.AttrProjectID:   cfg.ProjectID,
	}

	switch identity = strings.TrimSpace(identity); identity {
	case "none":
	case "":
		// git's *global* email: this lands in a global config and labels every future
		// session, so a repository-local address would follow the user out of the repo
		// it was set in. Resolved here and written as a literal — config holds strings,
		// not shell.
		if email := harness.GitEmail(ctx); email != "" {
			attrs[harness.AttrEnduserID] = email
		}
	default:
		attrs[harness.AttrEnduserID] = identity
	}
	return attrs
}

// printConflicts explains what is in the way, naming each variable so the user can go
// and look at it. A header value is never printed: it is the one that may hold someone
// else's credential. Every field is sanitized on the way to the terminal — a key can
// carry a file name, and a file name can carry anything.
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
		// Say it here as well as in the error: this is the one the user has to go
		// and fix themselves.
		if c.Scope == harness.ScopeUserSettings {
			fmt.Fprintf(out, "       Terma does not change this — it is your setting to remove.\n")
		} else {
			fmt.Fprintf(out, "       Terma cannot change this — it is outside the file Terma writes.\n")
		}
	}
}

// partitionConflicts separates the conflicts that gate a connect from the advisory
// ones, which are reported and nothing more.
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

// unclearable names the conflicts --force cannot resolve, which are fatal.
func unclearable(conflicts []harness.Conflict) []string {
	var out []string
	for _, c := range conflicts {
		if !c.Clearable {
			out = append(out, c.Key)
		}
	}
	return out
}

// backupHarnessConfig snapshots the file before it is merged, when the harness exposes
// a way to. Not part of the Harness interface: a harness whose config is not a single
// file it owns has nothing meaningful to snapshot.
func backupHarnessConfig(h harness.Harness, endpoint string) (string, error) {
	if b, ok := h.(harness.Backuper); ok {
		return b.Backup(endpoint)
	}
	return "", nil
}

// installStatusLine puts `terma hook statusline` in front of Claude Code's status
// line and returns the line to say about it. A failure is a warning, never a failed
// connect: the exporters are already written and working.
func installStatusLine(errOut io.Writer) string {
	c := harness.Claude{}
	changed, err := c.InstallStatusLine()
	if err != nil {
		fmt.Fprintf(errOut, "Warning: could not wrap Claude Code's status line (%v); plan usage will not be captured.\n", err)
		return ""
	}
	st, stErr := c.StatusLineState("")
	switch {
	case stErr != nil:
		return ""
	case changed && st.Renderer != "":
		return fmt.Sprintf("Status line: terma now reads the plan's usage windows from it; your own status line (%s) keeps running unchanged behind it.", output.SanitizeTerminal(st.Renderer))
	case changed:
		return "Status line: terma added one that shows model, context, cost and the plan's usage windows (remove it with `terma disconnect claude`, or skip it with --no-statusline)."
	default:
		return "Status line: already wrapped by terma."
	}
}

// confirm asks a yes/no question. It refuses to run without a terminal rather than
// assuming yes: this command writes a credential into a config file, and a piped
// invocation that meant to be non-interactive should say so with --yes.
func confirm(cmd *cobra.Command, question string) (bool, error) {
	if !output.Interactive() {
		return false, fmt.Errorf("%s — no terminal to confirm on; pass --yes to proceed non-interactively", question)
	}

	errOut := cmd.ErrOrStderr()
	p := style.For(errOut)
	fmt.Fprintf(errOut, "%s %s %s ", p.Brand("?"), p.Bold(question), p.Dim("[Y/n]"))
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil {
		// EOF with nothing typed is a decline, not a crash.
		if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
			return false, nil
		}
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true, nil
	default:
		return false, nil
	}
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

// signalLabel names a signal the way the docs do, so the plan reads as prose rather
// than as a list of OTLP nouns.
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
