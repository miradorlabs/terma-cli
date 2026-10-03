package claude

import (
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// ConflictsWith reports what in the existing config would defeat the export e describes:
// a generic endpoint already pointing elsewhere, a per-signal override, or the
// detailed-beta-tracing redirect.
func (c exporter) ConflictsWith(e harness.Exporter) ([]harness.Conflict, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return nil, err
	}
	s, err := loadSettings(path)
	if err != nil {
		return nil, err
	}
	return claudeConflicts(s.env, s.root, e, c.layer()), nil
}

// claudeConflicts finds every setting that would send data, and terma's key, somewhere other
// than e.Endpoint or break the export. Settings in l's own file are clearable; outranking ones are not.
func claudeConflicts(env map[string]string, root map[string]json.RawMessage, e harness.Exporter, l claudeLayer) []harness.Conflict {
	var out []harness.Conflict

	// Terma's own helper is the credential delivery this connect manages, not an override.
	if helper := stringSetting(root, claudeOtelHeadersHelper); helper != "" && !harness.IsOwnHelper(helper) {
		out = append(out, harness.Conflict{
			Key:        claudeOtelHeadersHelper,
			Value:      helper,
			Reason:     "supplies its own OTLP headers, which decide what the export authenticates with instead of Terma's key",
			Credential: true,
			Scope:      l.scope,
			Clearable:  true,
		})
	}

	// Not a disclosure, since it is overwritten, but it replaces an export the user chose, even a
	// disabled one.
	if current := env[harness.EnvOTLPEndpoint]; current != "" && current != e.Endpoint {
		reason := "telemetry is already exporting here; connecting replaces it"
		if !isOn(env[claudeEnableTelemetry]) {
			reason = "a previously configured destination; connecting replaces it"
		}
		out = append(out, harness.Conflict{
			Key: harness.EnvOTLPEndpoint, Value: current, Reason: reason,
			Scope: l.scope, Clearable: true,
		})
	}

	for _, o := range perSignalOverrides {
		if !e.HasSignal(o.signal) {
			continue
		}
		// A per-signal endpoint gets no `/v1/<signal>` appended, so the base URL posts to the wrong path.
		if v := env[o.endpoint]; v != "" && v != e.SignalEndpoint(o.signal) {
			reason := "overrides the endpoint for " + string(o.signal) + ", which would receive Terma's credential"
			if v == e.Endpoint {
				reason = "is Terma's base URL, which a per-signal endpoint does not append /v1/" +
					string(o.signal) + " to — " + string(o.signal) + " would post to the wrong path"
			}
			out = append(out, harness.Conflict{
				Key:        o.endpoint,
				Value:      v,
				Reason:     reason,
				Credential: v != e.Endpoint,
				Scope:      l.scope,
				Clearable:  true,
			})
		}
		if v := env[o.headers]; v != "" {
			// A header bag may hold a credential: named, never printed.
			out = append(out, harness.Conflict{
				Key:       o.headers,
				Reason:    "merges into the headers for " + string(o.signal) + ", overriding Terma's",
				Scope:     l.scope,
				Clearable: true,
			})
		}
		if v := env[o.protocol]; v != "" && v != harness.ProtocolHTTPProtobuf {
			out = append(out, harness.Conflict{
				Key:       o.protocol,
				Value:     v,
				Reason:    "sends " + string(o.signal) + " over a protocol Terma's endpoint does not serve",
				Scope:     l.scope,
				Clearable: true,
			})
		}
	}

	// Only both halves together redirect logs and traces; a saved endpoint with the switch off is
	// dormant, and flagging it would talk --force into deleting two settings for nothing.
	if endpoint := env[claudeBetaTracingEndpoint]; endpoint != "" &&
		isOn(env[claudeBetaTracingDetailed]) &&
		(e.HasSignal(harness.SignalTraces) || e.HasSignal(harness.SignalLogs)) {
		out = append(out, harness.Conflict{
			Key:    claudeBetaTracingEndpoint,
			Value:  endpoint,
			Reason: "detailed beta tracing sends logs and traces here instead of to Terma",
			// Undocumented whether it carries the OTLP headers; assumed yes, the safe side.
			Credential: true,
			Scope:      l.scope,
			Clearable:  true,
		})
	}

	// Outranking layers terma does not own.
	out = append(out, environmentConflicts(e)...)
	out = append(out, projectConflicts(e, l)...)
	out = append(out, captureConflictsIn(l)...)
	return out
}

// stringSetting reads a top-level string out of the raw document.
func stringSetting(root map[string]json.RawMessage, key string) string {
	raw, ok := root[key]
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}
