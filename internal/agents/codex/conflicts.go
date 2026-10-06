package codex

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// ConflictsWith reports what in the existing config would replace, defeat, or outrank
// the export e describes.
func (c exporter) ConflictsWith(e harness.Exporter) ([]harness.Conflict, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return nil, err
	}
	f, err := loadTOML(path)
	if err != nil {
		return nil, err
	}
	return codexConflicts(f, e), nil
}

// codexConflicts finds, per exported signal: an exporter pointing elsewhere (needs
// consent), the analytics opt-out, and the same keys in layers above the user config.
// Each exporter carries its own headers, so there is no credential-disclosure case.
func codexConflicts(f *tomlFile, e harness.Exporter) []harness.Conflict {
	var out []harness.Conflict

	for _, sk := range codexSignalKeys {
		if !e.HasSignal(sk.signal) {
			continue
		}
		shape := codexExporterOf(f.otel[sk.key])
		key := otelTable + "." + sk.key
		switch shape.Kind {
		case "", codexExporterNone, codexExporterStatsig:
			// Off, or Codex's own default route: nothing of the user's is replaced.
			continue
		case codexExporterOTLPHTTP:
			if shape.Endpoint == e.SignalEndpoint(sk.signal) {
				continue // a reconnect
			}
			reason := "already exports " + string(sk.signal) + " here; connecting replaces it"
			if strings.TrimRight(shape.Endpoint, "/") == strings.TrimRight(e.Endpoint, "/") {
				reason = "is Terma's base URL, which Codex posts to as-is — " + string(sk.signal) +
					" would go to the wrong path; connecting replaces it with " + e.SignalEndpoint(sk.signal)
			}
			out = append(out, harness.Conflict{
				Key: key, Value: shape.Endpoint, Reason: reason,
				Scope: harness.ScopeUserSettings, Clearable: true,
			})
		case codexExporterOTLPGRPC:
			out = append(out, harness.Conflict{
				Key:    key,
				Value:  shape.Endpoint,
				Reason: "already exports " + string(sk.signal) + " here over gRPC; connecting replaces it",
				Scope:  harness.ScopeUserSettings, Clearable: true,
			})
		default:
			out = append(out, harness.Conflict{
				Key:    key,
				Reason: "is set to something Terma does not recognize as an exporter; connecting replaces it",
				Scope:  harness.ScopeUserSettings, Clearable: true,
			})
		}
	}

	// The user's analytics opt-out is not Terma's to flip; refuse the metrics signal instead.
	if e.HasSignal(harness.SignalMetrics) && codexAnalyticsDisabled(f.doc) {
		out = append(out, harness.Conflict{
			Key:   codexAnalyticsTable + "." + codexAnalyticsEnabledKey,
			Value: "false",
			Reason: "Codex sends no metrics at all while analytics are disabled, so the metrics signal " +
				"would connect and deliver nothing — remove this setting, or connect with --signals traces,logs",
			Scope:     harness.ScopeUserSettings,
			Clearable: false,
		})
	}

	out = append(out, codexManagedConflicts(e)...)
	out = append(out, codexProfileConflicts(e)...)
	return out
}

type codexLayer struct {
	// source qualifies each conflict key so a report cannot mistake it for the user config.
	source string
	scope  string
	// where opens every reason: what the layer is and when Codex applies it.
	where string
	// advisory is set for a layer that applies only when the user selects it.
	advisory bool
}

// codexManagedConflicts reports managed_config.toml and the macOS MDM preference. A
// project's .codex/config.toml is not scanned: Codex refuses `otel` there. Windows's
// $CODEX_HOME copy, ignored by newer Codex builds, is read anyway.
func codexManagedConflicts(e harness.Exporter) []harness.Conflict {
	var out []harness.Conflict
	path := codexManagedConfigUnix
	if runtime.GOOS == "windows" {
		home, err := codexHome()
		if err != nil {
			return nil
		}
		path = filepath.Join(home, codexManagedConfigWindows)
	}
	if data, err := os.ReadFile(path); err == nil {
		out = append(out, codexConflictsInLayer(data, codexLayer{
			source: path,
			scope:  harness.ScopeManaged,
			where:  "set in " + path + ", which Codex applies over your user config",
		}, e)...)
	}
	if runtime.GOOS == "darwin" {
		if data, ok := codexManagedPreference(); ok {
			source := codexManagedPreferenceDomain + ":" + codexManagedPreferenceKey
			out = append(out, codexConflictsInLayer(data, codexLayer{
				source: source,
				scope:  harness.ScopeManaged,
				where:  "set by the managed preference " + source + ", which Codex applies over your user config",
			}, e)...)
		}
	}
	return out
}

// codexManagedPreference reads the managed-preferences domain Codex consults.
func codexManagedPreference() ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "defaults", "read", codexManagedPreferenceDomain, codexManagedPreferenceKey).Output()
	if err != nil {
		return nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, false
	}
	return decoded, true
}

// codexProfileConflicts scans every profile file, since which one a session selects is
// unknowable here; each finding is advisory.
func codexProfileConflicts(e harness.Exporter) []harness.Conflict {
	home, err := codexHome()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil
	}
	var out []harness.Conflict
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, codexProfileSuffix) || name == codexConfigFile {
			continue
		}
		profile := strings.TrimSuffix(name, codexProfileSuffix)
		path := filepath.Join(home, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		out = append(out, codexConflictsInLayer(data, codexLayer{
			source:   name,
			scope:    harness.ScopeProfile,
			where:    "set in " + path + ", which Codex applies over your user config only while that profile is selected with --profile " + profile,
			advisory: true,
		}, e)...)
	}
	return out
}

// codexConflictsInLayer reports what a higher layer overrides of e: an exporter for a
// selected signal (even "none"), a contradicting content switch, another attribution
// attribute, the analytics opt-out. Codex merges layers table by table.
func codexConflictsInLayer(data []byte, layer codexLayer, e harness.Exporter) []harness.Conflict {
	var doc struct {
		Otel      map[string]any `toml:"otel"`
		Analytics map[string]any `toml:"analytics"`
	}
	if unmarshalTOMLLenient(data, &doc) != nil {
		// A file this CLI cannot parse is Codex's to report.
		return nil
	}

	var out []harness.Conflict
	report := func(key, value, what string) {
		out = append(out, harness.Conflict{
			Key:       layer.source + ":" + key,
			Value:     value,
			Reason:    layer.where + ", and " + what,
			Scope:     layer.scope,
			Clearable: false,
			Advisory:  layer.advisory,
		})
	}

	for _, sk := range codexSignalKeys {
		if !e.HasSignal(sk.signal) {
			continue
		}
		v, ok := doc.Otel[sk.key]
		if !ok {
			continue
		}
		shape := codexExporterOf(v)
		if shape.Kind == codexExporterOTLPHTTP && shape.Endpoint == e.SignalEndpoint(sk.signal) {
			continue // the same destination, whichever layer names it
		}
		value := shape.Endpoint
		if value == "" {
			value = shape.Kind
		}
		report(otelTable+"."+sk.key, value, "decides where "+string(sk.signal)+" go")
	}

	// Content is always asked for: the team's policy, at the relay, withholds it.
	if v, ok := doc.Otel[codexLogUserPrompt].(bool); ok && !v {
		report(otelTable+"."+codexLogUserPrompt, strconv.FormatBool(v), "turns prompt capture "+switchWord(v))
	}
	// Codex's own record is the only copy of a tool's output, so a lower cap loses bytes.
	if table, ok := doc.Otel[codexToolResult].(map[string]any); ok {
		if n, ok := tomlInt(table[codexToolResultMaxBytes]); ok && n < codexContentLimit {
			reason := "turns tool output capture off"
			if n > 0 {
				reason = "cuts tool output at " + strconv.FormatInt(n, 10) + " bytes"
			}
			report(otelTable+"."+codexToolResult+"."+codexToolResultMaxBytes, strconv.FormatInt(n, 10), reason)
		}
	}
	if attrs, ok := doc.Otel[codexSpanAttributes].(map[string]any); ok {
		for _, key := range []string{harness.AttrEnduserID, semconv.MiradorProjectIDKey} {
			v, ok := attrs[key].(string)
			if !ok || v == e.ResourceAttributes[key] {
				continue
			}
			report(otelTable+"."+codexSpanAttributes+"."+key, v, "changes the "+key+" stamped on spans")
		}
	}
	if e.HasSignal(harness.SignalMetrics) {
		if v, ok := doc.Analytics[codexAnalyticsEnabledKey].(bool); ok && !v {
			report(codexAnalyticsTable+"."+codexAnalyticsEnabledKey, "false", "disables analytics, which silences metrics")
		}
	}
	return out
}

func switchWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// tomlInt reads an integer TOML value, tolerating the float a hand edit might produce.
func tomlInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}
