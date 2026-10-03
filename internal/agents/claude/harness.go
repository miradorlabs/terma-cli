package claude

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// exporter configures Claude Code's OpenTelemetry export through the `env` block of its
// settings file; the zero value acts on the user file, Local binds it to a repository.
type exporter struct {
	root string
}

// claudeProjectSettings is the committed project settings file, which Claude Code applies
// over the user file.
const claudeProjectSettings = ".claude/settings.json"

// claudeProjectLocalSettings is the gitignored per-developer project file; terma only scans it.
const claudeProjectLocalSettings = ".claude/settings.local.json"

// Local is the exporter bound to the repository at root.
func (exporter) Local(root string) (harness.Harness, bool) { return exporter{root: root}, true }

const (
	// claudeEnableTelemetry is the master switch; without it every OTEL_* variable is inert.
	claudeEnableTelemetry = "CLAUDE_CODE_ENABLE_TELEMETRY"
	// claudeEnhancedTelemetry gates spans: OTEL_TRACES_EXPORTER alone produces none.
	claudeEnhancedTelemetry = "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA"

	otelTracesExporter  = "OTEL_TRACES_EXPORTER"
	otelLogsExporter    = "OTEL_LOGS_EXPORTER"
	otelMetricsExporter = "OTEL_METRICS_EXPORTER"

	// The content switches are written either way, so the file states the redaction posture.
	otelLogUserPrompts       = "OTEL_LOG_USER_PROMPTS"
	otelLogAssistantResponse = "OTEL_LOG_ASSISTANT_RESPONSES"
	otelLogToolDetails       = "OTEL_LOG_TOOL_DETAILS"
	otelLogToolContent       = "OTEL_LOG_TOOL_CONTENT"

	// A disabled signal is written as "none" rather than omitted.
	exporterOTLP = "otlp"
	exporterNone = "none"

	// Together these send logs and traces to BETA_TRACING_ENDPOINT instead of the configured
	// exporters, bypassing OTEL_EXPORTER_OTLP_ENDPOINT, so a check of the OTLP variables misses it.
	claudeBetaTracingDetailed = "ENABLE_BETA_TRACING_DETAILED"
	claudeBetaTracingEndpoint = "BETA_TRACING_ENDPOINT"

	// claudeOtelHeadersHelper is a top-level setting, not an env entry, and supplies the same
	// Authorization header terma writes.
	claudeOtelHeadersHelper = "otelHeadersHelper"
)

// perSignalOverrides outrank the generic variables and receive the merged generic headers,
// so a stale one pointing at another vendor gets terma's key; terma reports them, never writes
// them, and removes them only when asked.
var perSignalOverrides = []struct {
	endpoint, protocol, headers string
	signal                      harness.Signal
}{
	{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", harness.SignalTraces},
	{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", harness.SignalLogs},
	{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", harness.SignalMetrics},
}

// claudeManagedKeys is every variable terma sets and exactly what disconnect removes; the
// per-signal overrides are not terma's and are absent on purpose.
var claudeManagedKeys = []string{
	claudeEnableTelemetry,
	claudeEnhancedTelemetry,
	otelTracesExporter,
	otelLogsExporter,
	otelMetricsExporter,
	harness.EnvOTLPProtocol,
	harness.EnvOTLPEndpoint,
	harness.EnvOTLPHeaders,
	otelLogUserPrompts,
	otelLogAssistantResponse,
	otelLogToolDetails,
	otelLogToolContent,
}

// claudeLocalKeys is what a repository-scope connect may switch off, clears and removes:
// never where, with which credential, or anything turned on. The beta switch is listed so an
// earlier terma's copy is removed.
var claudeLocalKeys = []string{
	claudeEnhancedTelemetry,
	otelTracesExporter,
	otelLogsExporter,
	otelMetricsExporter,
	otelLogUserPrompts,
	otelLogAssistantResponse,
	otelLogToolDetails,
	otelLogToolContent,
}

func (c exporter) managedKeys() []string {
	if c.root != "" {
		return claudeLocalKeys
	}
	return claudeManagedKeys
}

// Name is the token `--harness` accepts.
func (exporter) Name() string { return name }

// ServiceName is Claude Code's own default; terma writes no OTEL_RESOURCE_ATTRIBUTES.
func (exporter) ServiceName() string { return "claude-code" }

// DisplayName is how the agent is written in prose.
func (exporter) DisplayName() string { return displayName }

// Detect runs `claude --version`; a missing binary is not-found, not an error.
func (exporter) Detect(ctx context.Context) harness.Detection {
	return harness.DetectBinary(ctx, "claude", harness.SemverRE)
}

// ConfigPath is the user settings file (honouring CLAUDE_CONFIG_DIR, or a connect silently does
// nothing), or the repository's .claude/settings.json when bound to one.
func (c exporter) ConfigPath() (string, error) {
	if c.root != "" {
		return filepath.Join(c.root, filepath.FromSlash(claudeProjectSettings)), nil
	}
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// Status reads back what is currently installed.
func (c exporter) Status() (harness.Status, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return harness.Status{}, err
	}
	s, err := loadSettings(path)
	if err != nil {
		return harness.Status{}, err
	}

	status := harness.Status{
		ConfigPath: path,
		Exists:     s.existed,
		Endpoint:   s.env[harness.EnvOTLPEndpoint],
	}

	// The caller compares Endpoint against its own to decide whether this is terma.
	status.Connected = isOn(s.env[claudeEnableTelemetry]) && status.Endpoint != ""

	status.Signals = claudeSignals(s.env)
	// A repository can only switch things off, so what it leaves unsaid is the user level's.
	if c.root != "" {
		status.Signals = nil
		keys := map[harness.Signal]string{harness.SignalTraces: otelTracesExporter, harness.SignalLogs: otelLogsExporter, harness.SignalMetrics: otelMetricsExporter}
		for _, sig := range harness.AllSignals {
			if s.env[keys[sig]] != exporterNone {
				status.Signals = append(status.Signals, sig)
			}
		}
		// Only an off value is a policy; an earlier terma's on values are left to the next connect to clear.
		// A content key off is no policy any more, only an earlier terma's leftover to clear.
		for _, key := range claudeLocalKeys {
			switch v := s.env[key]; {
			case slices.Contains(captureKeys, key) && v == boolValue(false):
				status.StaleContent = true
			case v == exporterNone || v == boolValue(false):
				status.HasPolicy = true
			}
		}
	}

	status.KeyPrefix = maskKeyFromHeaders(s.env[harness.EnvOTLPHeaders])
	// In helper mode the key lives in the helper script.
	if status.KeyPrefix == "" {
		if helper := stringSetting(s.root, claudeOtelHeadersHelper); helper != "" && harness.IsOwnHelper(helper) {
			status.KeyPrefix = harness.MaskKey(harness.KeyFromHelper(helper))
		}
	}
	// With a journal, ownership is by value: a key edited since connect is the user's. A committed
	// repository policy may have no local journal.
	j, err := harness.LoadJournal(c.Name(), path)
	if err != nil {
		return harness.Status{}, err
	}
	status.ProjectID = projectIDOf(j)
	if j != nil {
		for key, installed := range j.Installed {
			if current, ok := s.env[key]; ok && current == installed {
				status.ManagedKeys++
			}
		}
		for key, installed := range j.InstalledSettings {
			if stringSetting(s.root, key) == installed {
				status.ManagedKeys++
			}
		}
	} else if c.root != "" {
		for _, key := range c.managedKeys() {
			if _, ok := s.env[key]; ok {
				status.ManagedKeys++
			}
		}
	}

	status.Conflicts = claudeConflicts(s.env, s.root, harness.Exporter{
		Endpoint: status.Endpoint,
		Signals:  status.Signals,
	}, c.layer())
	return status, nil
}

// Backup exposes the pre-modification copy. A journal decides whether it may be replaced, since
// the endpoint misjudges hand-written and edited configs; endpoint is the evidence without one.
func (c exporter) Backup(endpoint string) (string, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return "", err
	}
	s, err := loadSettings(path)
	if err != nil {
		return "", err
	}

	// A journal means the current contents are terma's, so the backup is still the pre-terma state.
	j, err := harness.LoadJournal(c.Name(), path)
	if err != nil {
		return "", err
	}
	if j != nil {
		return s.backup(false)
	}
	return s.backup(s.env[harness.EnvOTLPEndpoint] != endpoint)
}

// ManagedKeys is what Disconnect would remove, for a preview.
func (c exporter) ManagedKeys() []string { return c.managedKeys() }

// LocalOffOnly marks Claude Code's repository scope: project settings may only switch off.
func (exporter) LocalOffOnly() {}

var (
	_ harness.Harness      = exporter{}
	_ harness.LocalOffOnly = exporter{}
)
