package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

func (app *App) newTelemetryStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "status [" + strings.Join(app.agents.HarnessNames(), "|") + "]",
		Short:  "Show which harnesses are connected",
		Hidden: true,
		Long: `Reads each harness's own configuration and reports what is installed there.

This is the harness's view, not Terma's: it says what the harness is configured to
send, not whether anything has arrived. With no argument it reports every harness.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := app.loadConfig()
			if err != nil {
				return err
			}
			format, err := app.resolveFormat()
			if err != nil {
				return err
			}

			targets := app.agents.Harnesses()
			if len(args) == 1 {
				h, err := app.agents.Harness(args[0])
				if err != nil {
					return err
				}
				targets = []harness.Harness{h}
			}

			// A repository's local layer gets its own row under the global one.
			root, _ := localRoot(cmd.Context())

			rows := make([][]string, 0, len(targets))
			report := make([]telemetryStatus, 0, len(targets))
			for _, h := range targets {
				entry := describeStatus(cmd.Context(), h, cfg)
				report = append(report, entry)
				rows = append(rows, statusRow(h.Name(), entry))
				if local, ok := localLayerStatus(h, root, entry); ok {
					report = append(report, local)
					rows = append(rows, statusRow(h.Name()+" (local)", local))
				}
			}

			return output.Render(cmd.OutOrStdout(), format, output.Table{
				Headers: []string{"HARNESS", "INSTALLED", "TELEMETRY", "SIGNALS", "PROMPTS", "TOOL CONTENT"},
				Rows:    rows,
			}, telemetryStatusReport{Harnesses: report})
		},
	}
}

type telemetryStatusReport struct {
	Harnesses []telemetryStatus `json:"harnesses"`
}

// telemetryStatus carries only the key's masked head; the key is never rendered.
type telemetryStatus struct {
	Harness string `json:"harness"`
	// Scope is a harness.Scope; a local entry follows its global one.
	Scope       string `json:"scope"`
	Installed   string `json:"installed"`
	Version     string `json:"version,omitempty"`
	State       string `json:"state"`
	ConfigPath  string `json:"config_path,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	KeyPrefix   string `json:"key_prefix,omitempty"`
	Signals     string `json:"signals,omitempty"`
	Prompts     string `json:"prompts,omitempty"`
	ToolContent string `json:"tool_content,omitempty"`
	// Conflicts names per-signal overrides that send a signal, and the credential, elsewhere.
	Conflicts []string `json:"conflicts,omitempty"`
	// Warnings names overrides that apply only under an explicitly selected profile.
	Warnings []string `json:"warnings,omitempty"`
	Error    string   `json:"error,omitempty"`

	// exporting decides whether a repository's local layer is in effect or waiting.
	exporting bool
}

func describeStatus(ctx context.Context, h harness.Harness, cfg *config.Config) telemetryStatus {
	entry := telemetryStatus{Harness: h.Name(), Scope: string(harness.ScopeGlobal), Installed: "no"}

	detection := h.Detect(ctx)
	if detection.Found {
		entry.Installed = "yes"
		entry.Version = detection.Version
	}

	st, err := h.Status()
	if err != nil {
		// A state, not a failure: `terma harness status` must not exit non-zero for listing it.
		if _, ok := errors.AsType[*harness.ErrUnsupported](err); ok {
			entry.State = "unsupported"
			return entry
		}
		entry.State = "error"
		entry.Error = err.Error()
		return entry
	}

	entry.ConfigPath = st.ConfigPath
	entry.Endpoint = st.Endpoint
	entry.ProjectID = st.ProjectID
	entry.KeyPrefix = st.KeyPrefix
	blocking, advisory := partitionConflicts(st.Conflicts)
	for _, c := range blocking {
		entry.Conflicts = append(entry.Conflicts, c.Key)
	}
	for _, c := range advisory {
		entry.Warnings = append(entry.Warnings, c.Key)
	}

	switch {
	case !st.Connected:
		// Settings left behind still hold the key, so `disconnect` still has work to do.
		if st.ManagedKeys > 0 {
			entry.State = "settings present, not exporting"
			return entry
		}
		entry.State = "not connected"
		return entry
	case len(blocking) > 0:
		// Some signal is going somewhere else; the endpoint column alone would be a lie.
		entry.State = "connected, overridden"
		entry.exporting = true
	case st.Endpoint != cfg.OTLPURL:
		// Both endpoints, so the reader learns where without diffing JSON; a dev-connected
		// harness read under the prod profile lands here.
		entry.State = fmt.Sprintf("connected to %s (this profile expects %s)",
			output.SanitizeTerminal(st.Endpoint), output.SanitizeTerminal(cfg.OTLPURL))
	default:
		entry.State = "connected"
		entry.exporting = true
	}

	entry.Signals = joinSignals(st.Signals)
	if entry.Signals == "" {
		entry.Signals = "none"
	}
	entry.Prompts = onOff(st.IncludePrompts)
	entry.ToolContent = onOff(st.IncludeToolContent)
	return entry
}
