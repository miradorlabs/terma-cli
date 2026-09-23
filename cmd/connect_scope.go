package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/output"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/prompt"
)

// The interactive half of connect: a checklist of what to send and where, shown on a
// terminal before anything is planned. Every box has a flag — --signals,
// --exclude-prompts, --exclude-tool-content, --scope — so agents, CI and anyone who
// prefers typing get the same result without the form; --yes takes flags and defaults
// without asking. The form seeds from the flags, so a flag given on a terminal is the
// starting state rather than a way to skip the question.

// errCancelled is the user backing out of the checklist. Callers print "Cancelled" and
// return nil, the way a declined confirm does.
var errCancelled = errors.New("cancelled")

// canPrompt reports whether an interactive form can be shown: a human at a terminal,
// on both ends. output.Interactive covers stdout and agent detection; prompt.Interactive
// covers stdin, so `echo y | terma connect claude` falls through to the line-based
// confirm the way it always has.
func canPrompt() bool {
	return output.Interactive() && prompt.Interactive()
}

// signalDetails are the one-line explanations shown beside each signal's checkbox.
var signalDetails = map[harness.Signal]string{
	harness.SignalTraces:  "spans per turn, model call and tool call",
	harness.SignalLogs:    "prompts, tool calls and results, as events",
	harness.SignalMetrics: "what the usage and cost views are built on",
}

// connectForm is what the checklist needs to know beyond the flags. It asks what to
// send and where the file goes (global or local); which repositories a global connect
// exports from stays with --exports, since a connect naming one harness is already the
// expert path.
type connectForm struct {
	// root is the repository the CLI runs in, or "" outside one.
	root string
}

// askConnectOptions shows the checklist seeded from f and returns f with the answers.
func askConnectOptions(f connectFlags, hs []harness.Harness, ask connectForm) (connectFlags, error) {
	signals, err := harness.ParseSignals(f.signals)
	if err != nil {
		return f, err
	}
	scope, err := harness.ParseScope(f.scope)
	if err != nil {
		return f, err
	}
	if _, err := harness.ParseReach(f.exports); err != nil {
		return f, err
	}
	root := ask.root

	names := make([]string, 0, len(hs))
	for _, h := range hs {
		names = append(names, h.DisplayName())
	}
	form := &prompt.Form{Title: fmt.Sprintf("What should %s send to Terma?", joinNames(names))}
	for i, s := range harness.AllSignals {
		item := prompt.Item{
			Label:    signalLabel(s),
			Detail:   signalDetails[s],
			Selected: containsSignal(signals, s),
		}
		if i == 0 {
			item.Heading = "Data"
		}
		form.Items = append(form.Items, item)
	}
	form.Items = append(form.Items,
		prompt.Item{Label: "Prompt text and model responses", Detail: "what you and the model said", Selected: !f.excludePrompts},
		prompt.Item{Label: "Tool input and output", Detail: "commands run, files read, and what came back", Selected: !f.excludeToolContent},
	)
	const scopeGroup = 1
	unavailable := localUnavailable(hs, root)
	if scope == harness.ScopeLocal && unavailable != "" {
		// Asked for by flag and impossible here: say so rather than quietly flipping
		// the radio button to global.
		return f, errors.New("--scope local: " + unavailable)
	}
	local := prompt.Item{
		Kind: prompt.Radio, Group: scopeGroup, Label: "Local",
		Detail:   "this repository only — a committed project file; only what to ship",
		Selected: scope == harness.ScopeLocal,
		Disabled: unavailable != "", Reason: unavailable,
	}
	form.Items = append(form.Items,
		prompt.Item{
			Kind: prompt.Radio, Group: scopeGroup, Label: "Global", Heading: "Where",
			Detail:   "this machine, every repository — your user settings",
			Selected: !local.Selected,
		},
		local,
	)
	localAt := len(form.Items) - 1

	items, err := prompt.Run(form)
	if errors.Is(err, prompt.ErrCancelled) {
		return f, errCancelled
	}
	if err != nil {
		return f, err
	}

	var chosen []string
	for i, s := range harness.AllSignals {
		if items[i].Selected {
			chosen = append(chosen, string(s))
		}
	}
	f.signals = "none"
	if len(chosen) > 0 {
		f.signals = strings.Join(chosen, ",")
	}
	f.excludePrompts = !items[len(harness.AllSignals)].Selected
	f.excludeToolContent = !items[len(harness.AllSignals)+1].Selected
	f.scope = string(harness.ScopeGlobal)
	if items[localAt].Selected {
		f.scope = string(harness.ScopeLocal)
	}
	// Three ways to send nothing, and only one of them is a mistake. A repository that
	// ships nothing is a policy; a machine that hands the decision to its repositories
	// is the arrangement above. A machine that simply has every box unticked while
	// holding a live key is the mistake, and there is a better command for it.
	if len(chosen) == 0 && f.scope != string(harness.ScopeLocal) && f.exports != string(harness.ReachRepos) {
		return f, errors.New("nothing selected to send — tick at least one signal, or run `terma disconnect` to stop exporting")
	}
	return f, nil
}

// localUnavailable says why local scope cannot be offered, or "" when it can.
func localUnavailable(hs []harness.Harness, root string) string {
	if root == "" {
		return "not inside a git repository"
	}
	for _, h := range hs {
		if _, ok := h.(harness.Scoped); !ok {
			return h.DisplayName() + " has no repository settings"
		}
	}
	return ""
}

func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return "your coding agents"
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// localRoot is the repository the CLI runs in, which is what --scope local writes.
func localRoot(ctx context.Context) (string, error) {
	root, _, err := repoHere(ctx, "--scope local applies to a repository — run this inside a git checkout")
	return root, err
}

// localHarness binds h to the repository the CLI runs in, or explains why it cannot.
func localHarness(ctx context.Context, h harness.Harness) (harness.Harness, error) {
	scoped, ok := h.(harness.Scoped)
	if !ok {
		return nil, fmt.Errorf("%s has no repository settings — --scope local applies to harnesses that read one (Claude Code, OpenCode)", h.DisplayName())
	}
	root, err := localRoot(ctx)
	if err != nil {
		return nil, err
	}
	return scoped.Local(root), nil
}

// runLocalConnect writes a repository's policy: which signals and content its sessions
// ship, in its committed .claude/settings.json. Nothing about where or with which key —
// that stays in the global connect — so it needs no project, no sign-in and no network,
// and the file stays safe to commit.
func runLocalConnect(cmd *cobra.Command, name string, f connectFlags) error {
	global, err := harness.Lookup(name)
	if err != nil {
		return err
	}
	if f.apiKey != "" || f.keyName != "" || f.identity != "" || f.inlineKey {
		return errors.New("--api-key, --key-name, --identity and --inline-key belong to the global connect; a repository's settings carry only what to ship")
	}
	signals, err := harness.ParseSignals(f.signals)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	h, err := localHarness(ctx, global)
	if err != nil {
		return err
	}
	root, _ := localRoot(ctx)
	configPath, err := h.ConfigPath()
	if err != nil {
		return err
	}

	intended := harness.Exporter{
		// The endpoint is the global connect's and is not written here. It is carried
		// so a per-signal redirect in an outranking file is judged against where the
		// export actually goes.
		Endpoint:           cfg.OTLPURL,
		Signals:            signals,
		IncludePrompts:     !f.excludePrompts,
		IncludeToolContent: !f.excludeToolContent,
	}
	conflicts, err := h.ConflictsWith(intended)
	if err != nil {
		return err
	}

	printLocalConnectPlan(out, global, global.Detect(ctx), cfg, root, configPath, intended)
	printConflicts(out, conflicts, f.force)
	conflicts, _ = partitionConflicts(conflicts)
	if blocking := unclearable(conflicts); len(blocking) > 0 {
		return fmt.Errorf(
			"%s has settings Terma does not change: %s — remove or adjust them, then retry",
			global.DisplayName(), output.SanitizeTerminal(strings.Join(blocking, ", ")))
	}
	if len(conflicts) > 0 && !f.force {
		return fmt.Errorf(
			"%s already has OTLP settings in this repository that would override this connect — remove them, or pass --force to have Terma remove them",
			global.DisplayName())
	}

	if !f.assumeYes {
		ok, err := confirm(cmd, fmt.Sprintf("Write this repository's telemetry settings for %s?", global.DisplayName()))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, "Cancelled. Nothing was written.")
			return nil
		}
	}

	if err := h.Connect(intended, f.force); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nWritten %s.\n", configPath)
	fmt.Fprintf(out, "Commit it so every session in this repository ships the same data. Restart %s for it to take effect.\n", global.DisplayName())
	return nil
}

// printLocalConnectPlan is printConnectPlan for a repository layer: no project, no
// key, and the global connect it sits on named — a policy with nothing under it ships
// nothing, and the reader should learn that here, not from an empty dashboard.
func printLocalConnectPlan(
	out io.Writer,
	global harness.Harness,
	detection harness.Detection,
	cfg *config.Config,
	root, configPath string,
	e harness.Exporter,
) {
	printDetection(out, global, detection)
	fmt.Fprintf(out, "  Scope:       this repository (%s)\n", root)
	st, err := global.Status()
	if err == nil && st.Connected && st.Endpoint == cfg.OTLPURL {
		fmt.Fprintf(out, "  Exports via: your global %s connect (%s)\n", global.DisplayName(), st.Endpoint)
	} else {
		fmt.Fprintf(out, "  Exports via: nothing yet — %s is not connected to Terma on this machine.\n", global.DisplayName())
		fmt.Fprintf(out, "               This file decides what to ship; `terma connect %s` says where.\n", global.Name())
	}

	printDataPlan(out, e.Signals, e.IncludePrompts, e.IncludeToolContent)

	fmt.Fprintln(out, "\n  This will update:")
	fmt.Fprintf(out, "    %s  (what to ship — the endpoint and key stay in your user settings)\n", configPath)
	fmt.Fprintln(out)
}

// printDetection is the first line of every connect plan.
func printDetection(out io.Writer, h harness.Harness, detection harness.Detection) {
	if detection.Found {
		version := detection.Version
		if version == "" {
			version = "version unknown"
		}
		fmt.Fprintf(out, "%s found: %s\n", h.DisplayName(), version)
		return
	}
	// Not an error: the config is read whenever it is eventually started.
	fmt.Fprintf(out, "%s not found on PATH — the configuration will still be written.\n", h.DisplayName())
}

// printDataPlan lists what will be sent. The content lines are always printed,
// including when they are off — "off" is the answer to the question a reader has.
func printDataPlan(out io.Writer, signals []harness.Signal, prompts, toolContent bool) {
	fmt.Fprintln(out, "\n  Signals:")
	for _, s := range harness.AllSignals {
		mark := " "
		if containsSignal(signals, s) {
			mark = "✓"
		}
		fmt.Fprintf(out, "    %s %s\n", mark, signalLabel(s))
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "    Prompts:      %s\n", onOff(prompts))
	fmt.Fprintf(out, "    Tool content: %s\n", onOff(toolContent))
}

// scopeSuffix qualifies a "nothing to do" for the layer it was about.
func scopeSuffix(scope harness.Scope) string {
	if scope == harness.ScopeLocal {
		return " in this repository"
	}
	return ""
}

// localLayerStatus is the status row for a repository's local layer — present when the
// repository at root has one for h. The global row says whether anything is exported
// at all; this row says what this repository narrows it to, and whether that is in
// effect or waiting on a global connect.
func localLayerStatus(h harness.Harness, root string, global telemetryStatus) (telemetryStatus, bool) {
	scoped, ok := h.(harness.Scoped)
	if !ok || root == "" {
		return telemetryStatus{}, false
	}
	st, err := scoped.Local(root).Status()
	if err != nil || st.ManagedKeys == 0 {
		return telemetryStatus{}, false
	}
	entry := telemetryStatus{
		Harness:    h.Name(),
		Scope:      string(harness.ScopeLocal),
		Installed:  global.Installed,
		Version:    global.Version,
		ConfigPath: st.ConfigPath,
		State:      "local settings",
	}
	if !global.exporting {
		entry.State = "local settings, not exporting"
	}
	entry.Signals = joinSignals(st.Signals)
	if entry.Signals == "" {
		entry.Signals = "none"
	}
	entry.Prompts = onOff(st.IncludePrompts)
	entry.ToolContent = onOff(st.IncludeToolContent)
	return entry, true
}

func statusRow(name string, e telemetryStatus) []string {
	return []string{name, e.Installed, e.State, e.Signals, e.Prompts, e.ToolContent}
}

// describeShipment is a local layer in one clause, for `terma status`.
func describeShipment(st harness.Status) string {
	signals := "nothing"
	if len(st.Signals) > 0 {
		signals = joinSignals(st.Signals)
	}
	return fmt.Sprintf("%s; prompts %s; tool content %s", signals, onOff(st.IncludePrompts), onOff(st.IncludeToolContent))
}

// repoPolicyHarnesses is the subset of an install's adapters whose harness reads a
// repository's own export policy. Cursor and Codex are wired for hooks and nothing
// else: Cursor has no local OTLP exporter policy, and Codex ignores an otel table
// in a project's config.
func repoPolicyHarnesses(adapters []string) []harness.Harness {
	var out []harness.Harness
	for _, a := range adapters {
		h, err := harness.Lookup(a)
		if err != nil {
			continue
		}
		if _, ok := h.(harness.Scoped); ok {
			out = append(out, h)
		}
	}
	return out
}

// writeRepoPolicy writes each harness's repository-scope export policy: what this
// repository's sessions ship. No endpoint, no key, no identity — those belong to the
// developer's own settings, which is what keeps this file safe to commit.
//
// A conflict here is reported and skipped rather than fatal: the hooks and the binding
// are already written by this point, and failing the whole install over one harness's
// pre-existing OTLP settings would leave the repository half-onboarded for no gain.
func writeRepoPolicy(
	ctx context.Context,
	out io.Writer,
	root string,
	cfg *config.Config,
	hs []harness.Harness,
	f installFlags,
) ([]string, error) {
	signals, err := harness.ParseSignals(f.signals)
	if err != nil {
		return nil, err
	}
	var written []string
	for _, h := range hs {
		scoped, ok := h.(harness.Scoped)
		if !ok {
			continue
		}
		local := scoped.Local(root)
		status, err := local.Status()
		if err != nil {
			return nil, err
		}
		if status.HasPolicy && !f.updatePolicy {
			continue
		}
		intended := harness.Exporter{
			// Carried, never written: an outranking per-signal redirect has to be
			// judged against where the export actually goes.
			Endpoint:           cfg.OTLPURL,
			Signals:            signals,
			IncludePrompts:     !f.excludePrompts,
			IncludeToolContent: !f.excludeToolContent,
		}
		conflicts, err := local.ConflictsWith(intended)
		if err != nil {
			return nil, err
		}
		conflicts, _ = partitionConflicts(conflicts)
		if len(conflicts) > 0 {
			fmt.Fprintf(out, "\nSkipped %s's repository policy: %s already has OTLP settings here (%s).\n",
				h.DisplayName(), h.DisplayName(),
				output.SanitizeTerminal(strings.Join(conflictKeys(conflicts), ", ")))
			fmt.Fprintf(out, "Resolve them, then run `terma connect %s --scope local` in this repository.\n", h.Name())
			continue
		}
		path, err := local.ConfigPath()
		if err != nil {
			return nil, err
		}
		before, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := local.Connect(intended, false); err != nil {
			return nil, fmt.Errorf("write %s repository policy: %w", h.Name(), err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(before, after) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil, err
			}
			written = append(written, rel)
		}
		fmt.Fprintf(out, "\nWrote %s's repository policy to %s.\n", h.DisplayName(), path)
	}
	return written, nil
}

// conflictKeys names the settings in the way a reader can go and find them.
func conflictKeys(conflicts []harness.Conflict) []string {
	out := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		out = append(out, c.Key)
	}
	return out
}

// installedAdapters is the adapter list a repository recorded, falling back to the
// harnesses that read a repository policy so an uninstall still cleans up after an
// install whose binding has already been hand-edited away.
func installedAdapters(bound *termaproject.File) []string {
	if bound != nil && len(bound.Install.Adapters) > 0 {
		return bound.Install.Adapters
	}
	var out []string
	for _, h := range harness.All() {
		if _, ok := h.(harness.Scoped); ok {
			out = append(out, h.Name())
		}
	}
	return out
}
