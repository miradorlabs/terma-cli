package claude

import (
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// TracesOff names what keeps Claude Code's spans, claude_code.llm_request among them, from
// being exported: in the settings that apply at root, the beta switch with the traces
// exporter, both needed; with env, a running process's startup environment switching either
// off. Whether a settings entry beats the process's is undocumented, so either off loses them.
func (Agent) TracesOff(root string, env []string) []string {
	if env != nil {
		var off []string
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			if v != "" && (k == claudeEnhancedTelemetry && !isOn(v) || k == otelTracesExporter && v != exporterOTLP) {
				off = append(off, k)
			}
		}
		return off
	}
	st, err := exporter{}.EmissionStatus(root)
	if err != nil || slices.Contains(st.Signals, harness.SignalTraces) {
		return nil
	}
	return []string{claudeEnhancedTelemetry + "=1 with " + otelTracesExporter + "=" + exporterOTLP}
}

// SpansCarry is what only Claude Code's llm_request spans carry.
func (Agent) SpansCarry() string { return "time to first token, finish reason and attempts" }

var _ agents.TraceGated = Agent{}
