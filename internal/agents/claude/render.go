package claude

import (
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// render maps an Exporter onto Claude Code's variables. At repository scope only the off
// values remain: Claude Code ignores project settings that enable or redirect telemetry,
// and honours only those that switch a signal or content off.
func (c exporter) render(e harness.Exporter) map[string]string {
	env := renderClaude(e)
	if c.root == "" {
		return env
	}
	for key, value := range env {
		if !slices.Contains(claudeLocalKeys, key) || value != exporterNone && value != boolValue(false) {
			delete(env, key)
		}
	}
	return env
}

func renderClaude(e harness.Exporter) map[string]string {
	traces := e.HasSignal(harness.SignalTraces)

	env := map[string]string{
		claudeEnableTelemetry: "1",

		otelTracesExporter:  exporterFor(traces),
		otelLogsExporter:    exporterFor(e.HasSignal(harness.SignalLogs)),
		otelMetricsExporter: exporterFor(e.HasSignal(harness.SignalMetrics)),

		harness.EnvOTLPProtocol: harness.ProtocolHTTPProtobuf,
		harness.EnvOTLPEndpoint: e.Endpoint,

		otelLogUserPrompts:       boolValue(e.IncludePrompts),
		otelLogAssistantResponse: boolValue(e.IncludePrompts),
		otelLogToolDetails:       boolValue(e.IncludeToolContent),
		otelLogToolContent:       boolValue(e.IncludeToolContent),
	}

	// Only with traces on, so a logs-and-metrics connect opts no one into a beta.
	if traces {
		env[claudeEnhancedTelemetry] = "1"
	}

	// In helper mode the credential stays out of the settings file.
	if e.APIKey != "" && e.HelperPath == "" {
		env[harness.EnvOTLPHeaders] = "Authorization=Bearer " + e.APIKey
	}
	// e.ResourceAttributes are not rendered: OTEL_RESOURCE_ATTRIBUTES belongs to the user.
	return env
}

// renderedByTerma reports whether value is one renderClaude writes for a repository
// policy key: the only evidence a policy without a journal is Terma's to remove.
func renderedByTerma(key, value string) bool {
	switch key {
	case otelTracesExporter, otelLogsExporter, otelMetricsExporter:
		return value == exporterFor(true) || value == exporterFor(false)
	case otelLogUserPrompts, otelLogAssistantResponse, otelLogToolDetails, otelLogToolContent:
		return value == boolValue(true) || value == boolValue(false)
	case claudeEnhancedTelemetry:
		return value == "1"
	}
	return false
}

func exporterFor(on bool) string {
	if on {
		return exporterOTLP
	}
	return exporterNone
}

func boolValue(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

// isOn treats Claude Code's truthy spellings as on and everything else, including absent, as off.
func isOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
