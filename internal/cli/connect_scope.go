package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/connect"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// The connect checklist has a flag for every box and seeds from them, so a flag given on a
// terminal is the starting state rather than a way to skip the question.

// errCancelled is printed as "Cancelled" and returned as nil, like a declined confirm.
var errCancelled = errors.New("cancelled")

// canPrompt needs both halves: output.Interactive covers stdout and agent detection,
// prompt.Interactive stdin, so piped input falls through to the line-based confirm.
func canPrompt() bool {
	return output.Interactive() && prompt.Interactive()
}

var signalDetails = map[harness.Signal]string{
	harness.SignalTraces:  "spans per turn, model call and tool call",
	harness.SignalLogs:    "prompts, tool calls and results, as events",
	harness.SignalMetrics: "what the usage and cost views are built on",
}

// connectForm leaves --exports out of the checklist: a connect naming one harness is
// already the expert path.
type connectForm struct {
	root string
}

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
			Label:    connect.SignalLabel(s),
			Detail:   signalDetails[s],
			Selected: slices.Contains(signals, s),
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
		// Refused rather than quietly flipping the radio button to global.
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
	// Sending nothing is a policy locally and an arrangement under --exports repos; a
	// machine with every box unticked while holding a live key is the mistake.
	if len(chosen) == 0 && f.scope != string(harness.ScopeLocal) && f.exports != string(harness.ReachRepos) {
		return f, errors.New("nothing selected to send — tick at least one signal, or run `terma disconnect` to stop exporting")
	}
	return f, nil
}

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
	return cmp.Or(output.And(names), "your coding agents")
}

func localRoot(ctx context.Context) (string, error) {
	root, _, err := repoHere(ctx, "--scope local applies to a repository — run this inside a git checkout")
	return root, err
}

func (app *App) localHarness(ctx context.Context, h harness.Harness) (harness.Harness, error) {
	scoped, ok := h.(harness.Scoped)
	if !ok {
		return nil, fmt.Errorf("%s has no repository settings — --scope local applies to harnesses that read one (%s)", h.DisplayName(), app.scopedHarnessNames())
	}
	root, err := localRoot(ctx)
	if err != nil {
		return nil, err
	}
	return scoped.Local(root), nil
}

// runLocalConnect needs no project, sign-in or network: a repository's settings carry
// only what to ship.
func (app *App) runLocalConnect(cmd *cobra.Command, name string, f connectFlags) error {
	global, err := app.agents.Harness(name)
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
	cfg, err := app.loadConfig()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	h, err := app.localHarness(ctx, global)
	if err != nil {
		return err
	}
	root, _ := localRoot(ctx)
	return connect.Local(ctx, global, h, cfg, root, f.options(signals, harness.ReachEverywhere),
		connect.Steps{Confirm: func(q string) (bool, error) { return confirm(cmd, q) }},
		connect.IO{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()})
}

func scopeSuffix(scope harness.Scope) string {
	if scope == harness.ScopeLocal {
		return " in this repository"
	}
	return ""
}

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
	entry.Signals = connect.JoinSignals(st.Signals)
	if entry.Signals == "" {
		entry.Signals = "none"
	}
	entry.Prompts = connect.OnOff(st.IncludePrompts)
	entry.ToolContent = connect.OnOff(st.IncludeToolContent)
	return entry, true
}

func statusRow(name string, e telemetryStatus) []string {
	return []string{name, e.Installed, e.State, e.Signals, e.Prompts, e.ToolContent}
}

func describeShipment(st harness.Status) string {
	signals := "nothing"
	if len(st.Signals) > 0 {
		signals = connect.JoinSignals(st.Signals)
	}
	return fmt.Sprintf("%s; prompts %s; tool content %s", signals, connect.OnOff(st.IncludePrompts), connect.OnOff(st.IncludeToolContent))
}

// writeRepoPolicy skips a conflict rather than failing: the hooks and binding are already
// written, and failing would leave the repository half-onboarded.
func writeRepoPolicy(
	ctx context.Context,
	ui *installUI,
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
			// Carried, never written: an outranking per-signal redirect is judged against it.
			Endpoint:           cfg.OTLPURL,
			Signals:            signals,
			IncludePrompts:     !f.excludePrompts,
			IncludeToolContent: !f.excludeToolContent,
		}
		conflicts, err := local.ConflictsWith(intended)
		if err != nil {
			return nil, err
		}
		conflicts, _ = connect.Partition(conflicts)
		if len(conflicts) > 0 {
			ui.Warn("Repo policy", fmt.Sprintf("%s skipped — this repository already has OTLP settings for it (%s)",
				h.DisplayName(), output.SanitizeTerminal(strings.Join(conflictKeys(conflicts), ", "))))
			ui.Then(fmt.Sprintf("Resolve %s's OTLP settings in this repository, then run `terma connect %s --scope local` here.", h.DisplayName(), h.Name()))
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
		fmt.Fprintf(ui.detail, "\nWrote %s's repository policy to %s.\n", h.DisplayName(), path)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		ui.OK("Repo policy", h.DisplayName()+" telemetry settings in "+rel)
	}
	return written, nil
}

func conflictKeys(conflicts []harness.Conflict) []string {
	out := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		out = append(out, c.Key)
	}
	return out
}
