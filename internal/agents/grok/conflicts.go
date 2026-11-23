package grok

import (
	"net/url"
	"os"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// The shell variables grok reads ahead of its config.toml (grok 1.0.92, each one tried).
const (
	envLogsEndpoint    = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
	envLogsExporter    = "OTEL_LOGS_EXPORTER"
	envExternalOTel    = "GROK_EXTERNAL_OTEL"
	envLogsProtocol    = "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"
	envCompression     = "OTEL_EXPORTER_OTLP_COMPRESSION"
	envMetricsExporter = "OTEL_METRICS_EXPORTER"
	envMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
)

// contentSwitches are grok's content switches, which terma's config turns on and a shell
// export turns off.
var contentSwitches = []string{"OTEL_LOG_USER_PROMPTS", "OTEL_LOG_ASSISTANT_RESPONSES", "OTEL_LOG_TOOL_DETAILS", "OTEL_LOG_TOOL_CONTENT"}

// grokConflicts is what would defeat or redirect an export to the relay e, given table, the
// config's [telemetry]. Blocking: a shell export that sends grok's logs elsewhere, switches
// them off or breaks them, and a metrics exporter that would reach the relay (grok's metric
// points carry the developer's email). Advisory: a content switch turned off in the shell.
// The generic OTEL_EXPORTER_OTLP_ENDPOINT and …_PROTOCOL are none: the per-signal keys terma
// writes beat them. No value shows the relay's token.
func grokConflicts(e harness.Exporter, table map[string]any) []harness.Conflict {
	var out []harness.Conflict
	env := func(key, reason string, credential bool) {
		value := os.Getenv(key)
		out = append(out, harness.Conflict{Key: key, Value: relayexport.WithoutRelayToken(value, e.Endpoint),
			Reason: reason, Credential: credential, Scope: harness.ScopeEnvironment})
	}
	relayLogs := ""
	if e.Endpoint != "" {
		relayLogs = relayBase(e) + logsPath
	}
	if v := os.Getenv(envLogsEndpoint); v != "" && v != relayLogs {
		// Terma switched content on: prompts and tool content would go there.
		env(envLogsEndpoint, "exported in your shell, where grok reads it ahead of its config.toml — its logs, prompts and tool content included, go there, not to Terma", true)
	}
	if v := os.Getenv(envLogsExporter); v != "" && !namesOTLP(v) {
		env(envLogsExporter, "exported in your shell, where it switches grok's log export off", false)
	}
	if off(os.Getenv(envExternalOTel)) {
		env(envExternalOTel, "exported in your shell, where it switches grok's whole export off", false)
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv(envLogsProtocol))); v != "" && v != harness.ProtocolHTTPProtobuf {
		env(envLogsProtocol, "exported in your shell, where grok reads it ahead of its config.toml; the relay takes http/protobuf only, so the export fails", false)
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv(envCompression)), "none") {
		env(envCompression, "exported in your shell, where grok rejects it and switches its export off", false)
	}
	if c, ok := metricsToRelay(e, table); ok {
		out = append(out, c)
	}
	for _, key := range contentSwitches {
		if off(os.Getenv(key)) {
			out = append(out, harness.Conflict{Key: key, Value: os.Getenv(key), Scope: harness.ScopeEnvironment, Advisory: true,
				Reason: "exported in your shell, where it turns that content off; the team's policy decides what leaves, so terma asks for all of it"})
		}
	}
	return out
}

// metricsToRelay is a metrics exporter that would send grok's metrics to the relay: one
// naming otlp, the shell's ahead of the config's, while the metrics endpoint in effect (the
// shell's per-signal one, the config's, the shell's generic one, the config's) is on the
// relay's address.
func metricsToRelay(e harness.Exporter, table map[string]any) (harness.Conflict, bool) {
	relay, err := url.Parse(e.Endpoint)
	if e.Endpoint == "" || err != nil || relay.Host == "" {
		return harness.Conflict{}, false
	}
	c := harness.Conflict{Key: envMetricsExporter, Value: os.Getenv(envMetricsExporter), Scope: harness.ScopeEnvironment}
	if c.Value == "" {
		c.Key, c.Scope = telemetryTable+"."+keyMetricsExporter, harness.ScopeUserSettings
		c.Value, _ = table[keyMetricsExporter].(string)
	}
	if !namesOTLP(c.Value) {
		return harness.Conflict{}, false
	}
	configured := func(key string) string { s, _ := table[key].(string); return s }
	for _, endpoint := range []struct{ key, value string }{
		{envMetricsEndpoint, os.Getenv(envMetricsEndpoint)},
		{telemetryTable + "." + keyMetricsEndpoint, configured(keyMetricsEndpoint)},
		{harness.EnvOTLPEndpoint, os.Getenv(harness.EnvOTLPEndpoint)},
		{telemetryTable + "." + keyEndpoint, configured(keyEndpoint)},
	} {
		if endpoint.value == "" {
			continue
		}
		if u, err := url.Parse(endpoint.value); err != nil || u.Host != relay.Host {
			return harness.Conflict{}, false
		}
		where := "exported in your shell"
		if c.Scope == harness.ScopeUserSettings {
			where = "set in grok's config.toml"
		}
		c.Reason = where + ", where it switches grok's metric export on, which " + endpoint.key + " (" +
			relayexport.WithoutRelayToken(endpoint.value, e.Endpoint) + ") sends to the relay; grok's metric points carry your email"
		return c, true
	}
	return harness.Conflict{}, false
}

// namesOTLP reports whether an exporter list names otlp.
func namesOTLP(list string) bool {
	for e := range strings.SplitSeq(list, ",") {
		if strings.EqualFold(strings.TrimSpace(e), exporterOTLP) {
			return true
		}
	}
	return false
}

// off reports whether a switch's value turns it off.
func off(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}
