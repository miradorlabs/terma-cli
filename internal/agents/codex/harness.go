package codex

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// exporter configures the exporter CLI's export through the `[otel]` table of config.toml.
// exporter reads no OTEL_* variables and has no headers helper, so the key is written
// inline and the file tightened to 0600.
type exporter struct{}

const (
	// `exporter` is the log exporter; metrics defaults to "statsig", OpenAI's own
	// analytics, which connecting the metrics signal replaces.
	codexLogExporter     = "exporter"
	codexTraceExporter   = "trace_exporter"
	codexMetricsExporter = "metrics_exporter"

	// codexLogUserPrompt is the only content switch: Codex never exports response text.
	codexLogUserPrompt = "log_user_prompt"

	// codexToolResult caps tool output bytes (2048 upstream, zero drops it); tool
	// arguments are logged regardless.
	codexToolResult         = "tool_result"
	codexToolResultMaxBytes = "max_bytes"

	// codexSpanAttributes is the only per-user attribute Codex accepts, on spans only.
	// Terma owns entries, never the table, keyed `span_attributes/<attribute>` in the flat
	// view; a slash cannot appear in an otel key, so the split is unambiguous.
	codexSpanAttributes      = "span_attributes"
	codexSpanAttributePrefix = codexSpanAttributes + "/"

	// With analytics `enabled` false Codex installs no metrics exporter at all, whatever
	// `metrics_exporter` says.
	codexAnalyticsTable      = "analytics"
	codexAnalyticsEnabledKey = "enabled"

	codexExporterNone     = "none"
	codexExporterStatsig  = "statsig"
	codexExporterOTLPHTTP = "otlp-http"
	codexExporterOTLPGRPC = "otlp-grpc"

	codexEndpointKey = "endpoint"
	codexHeadersKey  = "headers"
	codexProtocolKey = "protocol"
	// codexProtocolBinary is http/protobuf in Codex's spelling.
	codexProtocolBinary = "binary"

	codexAuthorizationHeader = "Authorization"

	codexServiceName = "codex_cli_rs"

	codexConfigFile = "config.toml"
	// Profile files layer over config.toml only while selected with --profile.
	codexProfileSuffix = ".config.toml"

	// Administrator-managed layers outrank the user file; the macOS managed preference is a
	// base64-encoded config.toml.
	codexManagedConfigUnix       = "/etc/codex/managed_config.toml"
	codexManagedConfigWindows    = "managed_config.toml"
	codexManagedPreferenceDomain = "com.openai.codex"
	codexManagedPreferenceKey    = "config_toml_base64"
)

// codexSignalKeys maps each signal to its exporter key, in report order.
var codexSignalKeys = []struct {
	signal harness.Signal
	key    string
}{
	{harness.SignalTraces, codexTraceExporter},
	{harness.SignalLogs, codexLogExporter},
	{harness.SignalMetrics, codexMetricsExporter},
}

// Name is the token `terma connect` and `--harness` accept.
func (exporter) Name() string { return name }

// ServiceName is codex_cli_rs, the originator Codex stamps for its CLI; Desktop and the
// IDE extensions report their own.
func (exporter) ServiceName() string { return codexServiceName }

// DisplayName is how the agent is written in prose.
func (exporter) DisplayName() string { return displayName }

// SupportsHeadersHelper is false: Codex's headers are literal strings in config.toml.
func (exporter) SupportsHeadersHelper() bool { return false }

// Detect runs `codex --version`. A missing binary is not-found rather than an error.
func (exporter) Detect(ctx context.Context) harness.Detection {
	return harness.DetectBinary(ctx, "codex", harness.SemverRE)
}

// codexHome honours $CODEX_HOME: writing where Codex does not read is a connect that
// silently does nothing.
func codexHome() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// ConfigPath is $CODEX_HOME/config.toml.
func (exporter) ConfigPath() (string, error) {
	dir, err := codexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, codexConfigFile), nil
}

// Status reads back what is currently installed.
func (c exporter) Status() (harness.Status, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return harness.Status{}, err
	}
	f, err := loadTOML(path)
	if err != nil {
		return harness.Status{}, err
	}

	status := harness.Status{
		ConfigPath:         path,
		Exists:             f.existed,
		IncludePrompts:     f.otel[codexLogUserPrompt] == true,
		IncludeToolContent: codexToolContentOn(f.otel),
		ProjectID:          codexSpanAttribute(f.otel, harness.AttrProjectID),
	}

	// Codex has no switch beyond the exporters, so an OTLP exporter present is telemetry on.
	for _, sk := range codexSignalKeys {
		shape := codexExporterOf(f.otel[sk.key])
		if shape.Kind != codexExporterOTLPHTTP && shape.Kind != codexExporterOTLPGRPC {
			continue
		}
		base := codexBaseEndpoint(shape.Endpoint, sk.signal)
		if status.Endpoint == "" {
			status.Endpoint = base
		}
		if codexTermaExporter(f.otel[sk.key], status.Endpoint, sk.signal) {
			status.Signals = append(status.Signals, sk.signal)
			// Only a Terma key is named: the exporter may carry somebody else's bearer token.
			if key := codexKeyFromExporter(f.otel[sk.key]); status.KeyPrefix == "" && serverkey.Is(key) {
				status.KeyPrefix = harness.MaskKey(key)
			}
		}
	}
	status.Connected = status.Endpoint != ""

	// Computed before the analytics check below drops metrics.
	status.Conflicts = codexConflicts(f, harness.Exporter{
		Endpoint:           status.Endpoint,
		Signals:            status.Signals,
		IncludePrompts:     status.IncludePrompts,
		IncludeToolContent: status.IncludeToolContent,
		ResourceAttributes: map[string]string{
			harness.AttrEnduserID: codexSpanAttribute(f.otel, harness.AttrEnduserID),
			harness.AttrProjectID: status.ProjectID,
		},
	})
	// A metrics exporter with analytics disabled sends nothing.
	if codexAnalyticsDisabled(f.doc) {
		status.Signals = harness.WithoutSignal(status.Signals, harness.SignalMetrics)
	}

	// Ownership is by journal only: a config without one is somebody else's (a company
	// collector, say), and disconnect must not delete it.
	j, err := harness.LoadJournal(c.Name(), path)
	if err != nil {
		return harness.Status{}, err
	}
	if j != nil {
		current, err := canonicalOtel(f.otel)
		if err != nil {
			return harness.Status{}, err
		}
		for key, installed := range j.Installed {
			if current[key] == installed {
				status.ManagedKeys++
			}
		}
	}
	return status, nil
}

// Connect merges Terma's keys into the otel table, leaving everything else as it was.
// A key an earlier connect wrote and this one does not goes back to its pre-Terma value
// while it still holds Terma's, or a narrower reconnect would keep sending more.
func (c exporter) Connect(e harness.Exporter, clearConflicts bool) error {
	rendered := c.render(e)

	path, err := c.ConfigPath()
	if err != nil {
		return err
	}
	f, err := loadTOML(path)
	if err != nil {
		return err
	}
	previousJournal, err := harness.LoadJournal(c.Name(), path)
	if err != nil {
		return err
	}
	current, err := canonicalOtel(f.otel)
	if err != nil {
		return err
	}

	cleared := map[string]string{}
	if clearConflicts {
		for _, conflict := range codexConflicts(f, e) {
			if !conflict.Clearable {
				continue
			}
			key := strings.TrimPrefix(conflict.Key, otelTable+".")
			// Overwritten by the merge anyway; the journal keeps the prior value for disconnect.
			if _, overwritten := rendered[key]; overwritten {
				continue
			}
			if value, ok := current[key]; ok {
				cleared[key] = value
			}
			delete(current, key)
		}
	}

	// carried drops the keys restored here, so the new journal does not own them.
	carried := previousJournal
	if previousJournal != nil {
		copied := *previousJournal
		copied.Installed = maps.Clone(previousJournal.Installed)
		copied.Previous = maps.Clone(previousJournal.Previous)
		for key, installed := range previousJournal.Installed {
			if _, writes := rendered[key]; writes {
				continue
			}
			if current[key] != installed {
				continue // edited since; theirs now, and named on disconnect
			}
			if prior := previousJournal.Previous[key]; prior != nil {
				current[key] = *prior
			} else {
				delete(current, key)
			}
			delete(copied.Installed, key)
			delete(copied.Previous, key)
		}
		carried = &copied
	}

	j := harness.NewJournal(c.Name(), path, current, rendered, cleared, nil, carried)

	maps.Copy(current, rendered)
	f.otel, err = otelFromCanonical(current)
	if err != nil {
		return err
	}

	// Ownership first, then the file; a failed write restores the previous journal.
	if err := j.Save(); err != nil {
		return err
	}
	defer harness.PruneJournals()
	if err := f.save(e.APIKey != ""); err != nil {
		var rollbackErr error
		if previousJournal != nil {
			rollbackErr = previousJournal.Save()
		} else {
			rollbackErr = harness.DeleteJournal(c.Name(), path)
		}
		if rollbackErr != nil {
			return fmt.Errorf("write config: %w (also failed to restore telemetry journal: %w)", err, rollbackErr)
		}
		return err
	}
	return nil
}

// Disconnect undoes the recorded connect; without a journal nothing here is Terma's.
func (c exporter) Disconnect() (harness.DisconnectResult, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return harness.DisconnectResult{}, err
	}
	f, err := loadTOML(path)
	if err != nil {
		return harness.DisconnectResult{}, err
	}
	j, err := harness.LoadJournal(c.Name(), path)
	if err != nil {
		return harness.DisconnectResult{}, err
	}
	current, err := canonicalOtel(f.otel)
	if err != nil {
		return harness.DisconnectResult{}, err
	}

	if j == nil {
		return harness.DisconnectResult{}, nil
	}
	result, remaining := j.Apply(current)
	slices.Sort(result.Skipped)

	defer harness.PruneJournals()
	if result.Removed > 0 || result.Restored > 0 {
		f.otel, err = otelFromCanonical(current)
		if err != nil {
			return result, err
		}
		if err := f.save(false); err != nil {
			return result, err
		}
	}

	if remaining != nil && !remaining.Empty() {
		return result, remaining.Save()
	}
	return result, harness.DeleteJournal(c.Name(), path)
}

// Backup exposes the pre-modification copy: kept when a journal makes the file Terma's
// work, otherwise only when the config points at endpoint.
func (c exporter) Backup(endpoint string) (string, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return "", err
	}
	f, err := loadTOML(path)
	if err != nil {
		return "", err
	}
	j, err := harness.LoadJournal(c.Name(), path)
	if err != nil {
		return "", err
	}
	if j != nil {
		return f.backup(false)
	}
	pointsAtTerma := false
	for _, sk := range codexSignalKeys {
		if codexTermaExporter(f.otel[sk.key], endpoint, sk.signal) {
			pointsAtTerma = true
		}
	}
	return f.backup(!pointsAtTerma)
}

// ConnectNotes says that connecting metrics takes them from OpenAI's own route and that
// excluding tool content cannot exclude tool arguments.
func (exporter) ConnectNotes(e harness.Exporter) []string {
	var notes []string
	if e.HasSignal(harness.SignalMetrics) {
		notes = append(notes, "Codex sends metrics to OpenAI (statsig) unless configured otherwise; after this connect they go to Terma instead.")
	}
	if !e.IncludeToolContent {
		notes = append(notes, "Codex has no switch for tool arguments: tool output is dropped, but the command or parameters of each tool call are still exported.")
	}
	return notes
}

// CurrentCredential returns the key installed for both endpoint and projectID, so a
// reconnect reuses it instead of minting an orphan.
func (c exporter) CurrentCredential(endpoint, projectID string) (string, bool) {
	path, err := c.ConfigPath()
	if err != nil {
		return "", false
	}
	f, err := loadTOML(path)
	if err != nil {
		return "", false
	}
	if codexSpanAttribute(f.otel, harness.AttrProjectID) != projectID {
		return "", false
	}
	for _, sk := range codexSignalKeys {
		if !codexTermaExporter(f.otel[sk.key], endpoint, sk.signal) {
			continue
		}
		if key := codexKeyFromExporter(f.otel[sk.key]); serverkey.Is(key) {
			return key, true
		}
	}
	return "", false
}

// A drifted optional method is a build error here, not a silent switch-off.
// Local is unsupported: Codex strips `otel` from a project's config.
func (exporter) Local(string) (harness.Harness, bool) { return nil, false }

var (
	_ harness.Harness = exporter{}
)
