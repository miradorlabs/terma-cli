package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/output"
)

func newTelemetryStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "status [" + strings.Join(harness.Names(), "|") + "]",
		Short:  "Show which harnesses are connected",
		Hidden: true,
		Long: `Reads each harness's own configuration and reports what is installed there.

This is the harness's view, not Terma's: it says what the harness is configured to
send, not whether anything has arrived. With no argument it reports every harness.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			format, err := resolveFormat()
			if err != nil {
				return err
			}

			targets := harness.All()
			if len(args) == 1 {
				h, err := harness.Lookup(args[0])
				if err != nil {
					return err
				}
				targets = []harness.Harness{h}
			}

			// A repository's local layer gets a row of its own under the global one: what
			// the user file says, then what this repository narrows it to. Only when run
			// inside a repository that has one.
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

// telemetryStatus is the machine-readable view. KeyPrefix is the masked head only —
// the key itself is never rendered, in any format.
type telemetryStatus struct {
	Harness string `json:"harness"`
	// Scope is global for the harness's user-level file, local for a repository's own
	// layer (harness.Scope). A local entry follows its global one.
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
	// Conflicts names the per-signal overrides that make Endpoint above only part of
	// the truth. Reporting a Terma endpoint while a per-signal override quietly sends
	// that signal — and the credential — elsewhere is the failure this exists to prevent.
	Conflicts []string `json:"conflicts,omitempty"`
	// Warnings names overrides that apply only in a mode the user selects explicitly —
	// a Codex profile. They do not make the harness "overridden", since whether they
	// apply to the next session is not knowable here.
	Warnings []string `json:"warnings,omitempty"`
	Error    string   `json:"error,omitempty"`

	// exporting is whether telemetry reaches this profile's Terma endpoint at all —
	// what decides whether a repository's local layer is in effect or waiting.
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
		// An unsupported harness is a state, not a failure — reporting it as an error
		// would make `terma harness status` exit non-zero just for listing Codex.
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
		// Settings left behind after the switch was turned off still hold the key, and
		// `disconnect` still has work to do — so this is not the same as "nothing here".
		if st.ManagedKeys > 0 {
			entry.State = "settings present, not exporting"
			return entry
		}
		entry.State = "not connected"
		return entry
	case len(blocking) > 0:
		// Say this rather than "connected": some signal is going somewhere else, and the
		// endpoint column alone would be a lie.
		entry.State = "connected, overridden"
		entry.exporting = true
	case st.Endpoint != cfg.OTLPURL:
		// Telemetry is on, but aimed somewhere other than this profile's endpoint.
		// Saying "connected" would be wrong in the way that costs the most time to
		// discover — and naming only the state would leave the reader diffing JSON to
		// learn *where*. Both endpoints in one line: the harness's actual destination,
		// and the one the active profile expected. A dev-connected harness read under
		// the prod profile is the everyday way to land here.
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
