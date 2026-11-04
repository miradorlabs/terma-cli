package claude

import (
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// claudeEnhancedTelemetryAlias is the beta switch's other name, which Claude Code accepts too.
const claudeEnhancedTelemetryAlias = "ENABLE_ENHANCED_TELEMETRY_BETA"

// TracesOff names what in the user settings file keeps Claude Code's spans, claude_code.llm_request
// among them, from being exported: the master switch, the beta switch and the traces exporter, all
// needed. Repository and managed settings are left out: either switching traces off is its own
// policy, which terma setup does not change.
// Accepted risk: this is the file terma's own CLAUDE_CONFIG_DIR names, as for setup and every
// other check, so a running process started with another is judged by this file; reading that
// process's own settings through TracesOffIn is the upgrade path.
func (Agent) TracesOff() ([]string, error) {
	path, err := exporter{}.ConfigPath()
	if err != nil {
		return nil, err
	}
	s, err := loadSettings(path)
	if err != nil {
		return nil, err
	}
	var off []string
	if !isOn(s.env[claudeEnableTelemetry]) {
		off = append(off, claudeEnableTelemetry+"=1")
	}
	if !isOn(s.env[claudeEnhancedTelemetry]) && !isOn(s.env[claudeEnhancedTelemetryAlias]) {
		off = append(off, claudeEnhancedTelemetry+"=1")
	}
	if !exportsOTLP(s.env[otelTracesExporter]) {
		off = append(off, otelTracesExporter+"="+exporterOTLP)
	}
	return off, nil
}

// TracesOffIn names the variables of env, a running process's startup environment, that switch
// Claude Code's spans off; an empty one is unset. As in terma's conflict checks, the process
// environment is taken to outrank the user settings file.
func (Agent) TracesOffIn(env []string) []string {
	vars := map[string]string{}
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); v != "" {
			vars[k] = v
		}
	}
	var off []string
	if v := vars[claudeEnableTelemetry]; v != "" && !isOn(v) {
		off = append(off, claudeEnableTelemetry)
	}
	// Either name on is enough, and settings name the beta switch as setup writes it, so only
	// that name off, the other not on, can switch it off here.
	if v := vars[claudeEnhancedTelemetry]; v != "" && !isOn(v) && !isOn(vars[claudeEnhancedTelemetryAlias]) {
		off = append(off, claudeEnhancedTelemetry)
	}
	if v := vars[otelTracesExporter]; v != "" && !exportsOTLP(v) {
		off = append(off, otelTracesExporter)
	}
	return off
}

// SpansCarry is what only Claude Code's llm_request spans carry; its api_request log carries time to first token too.
func (Agent) SpansCarry() string { return "finish reason and attempts" }

var _ agents.TraceGated = Agent{}

// exportsOTLP reports whether an exporter list, comma-separated, names OTLP.
func exportsOTLP(exporters string) bool {
	return slices.ContainsFunc(strings.Split(exporters, ","), func(e string) bool { return strings.TrimSpace(e) == exporterOTLP })
}
