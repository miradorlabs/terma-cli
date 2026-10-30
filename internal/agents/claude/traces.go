package claude

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// TracesOff names what in the user settings file keeps Claude Code's spans, claude_code.llm_request
// among them, from being exported: the master switch, the beta switch and the traces exporter, all
// needed. A repository's settings are left out: one switching traces off is its own policy.
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
	for _, key := range []string{claudeEnableTelemetry, claudeEnhancedTelemetry} {
		if !isOn(s.env[key]) {
			off = append(off, key+"=1")
		}
	}
	if s.env[otelTracesExporter] != exporterOTLP {
		off = append(off, otelTracesExporter+"="+exporterOTLP)
	}
	return off, nil
}

// TracesOffIn names the variables of env, a running process's startup environment, that switch
// Claude Code's spans off. Whether a settings entry beats the process's is undocumented, so
// either off may lose them; an empty one is unset.
func (Agent) TracesOffIn(env []string) []string {
	var off []string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if v != "" && (k == claudeEnableTelemetry && !isOn(v) || k == claudeEnhancedTelemetry && !isOn(v) || k == otelTracesExporter && v != exporterOTLP) {
			off = append(off, k)
		}
	}
	return off
}

// SpansCarry is what only Claude Code's llm_request spans carry; its api_request log carries time to first token too.
func (Agent) SpansCarry() string { return "finish reason and attempts" }

var _ agents.TraceGated = Agent{}
