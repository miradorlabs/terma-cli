package codex

import (
	"fmt"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// Render maps an Exporter onto the otel table as TOML text, entry tables one entry at a
// time; an unselected signal is left as it was (for metrics, OpenAI's own route). Prompts
// are always logged and tool output kept to terma's own content bound, not Codex's 2 KiB:
// the relay withholds content per the team's policy.
func (exporter) render(e harness.Exporter) map[string]string {
	out := map[string]string{
		codexLogUserPrompt: mustRenderTOML(true),
		codexToolResult + "/" + codexToolResultMaxBytes: mustRenderTOML(codexContentLimit),
	}
	for _, sk := range codexSignalKeys {
		if e.HasSignal(sk.signal) {
			out[sk.key] = mustRenderTOML(codexOTLPExporter(e, sk.signal))
		}
	}
	for key, value := range codexSpanAttributeValues(e.ResourceAttributes) {
		out[codexSpanAttributePrefix+key] = mustRenderTOML(value)
	}
	return out
}

// RuntimeArgs configures one launch through -c overrides; they carry Authorization, so
// callers must never log them.
func (c exporter) RuntimeArgs(e harness.Exporter) []string {
	values := c.render(e)
	// Codex splits override paths on every dot without TOML quoting, so an entry table,
	// whose dotted attribute names would split, travels as one inline table value.
	for _, table := range codexEntryTables {
		entries := map[string]any{}
		for k, text := range values {
			if entry, ok := strings.CutPrefix(k, table+"/"); ok {
				v, err := parseTOMLValue(text)
				if err != nil {
					panic(err) // render's own text always parses
				}
				entries[entry] = v
				delete(values, k)
			}
		}
		if len(entries) > 0 {
			values[table] = mustRenderTOML(entries)
		}
	}
	// Unselected signals are turned off explicitly: otherwise Codex falls through to the
	// user-level config, which may export to another project.
	for _, key := range []string{codexLogExporter, codexTraceExporter, codexMetricsExporter} {
		if _, ok := values[key]; !ok {
			values[key] = mustRenderTOML(codexExporterNone)
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	args := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		args = append(args, "-c", "otel."+k+"="+values[k])
	}
	if e.HasSignal(harness.SignalMetrics) {
		args = append(args, "-c", "analytics.enabled=true")
	}
	return args
}

// codexOTLPExporter is otlp-http with the binary protocol at the signal URL: Codex uses
// an exporter endpoint as-is, so it must carry /v1/<signal>.
func codexOTLPExporter(e harness.Exporter, s harness.Signal) map[string]any {
	inner := map[string]any{
		codexEndpointKey: e.SignalEndpoint(s),
		codexProtocolKey: codexProtocolBinary,
	}
	if e.APIKey != "" {
		inner[codexHeadersKey] = map[string]any{codexAuthorizationHeader: "Bearer " + e.APIKey}
	}
	return map[string]any{codexExporterOTLPHTTP: inner}
}

// codexSpanAttributeValues omits service.name, which Codex stamps on the resource itself.
func codexSpanAttributeValues(attrs map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range attrs {
		if k == "" || v == "" || k == harness.AttrServiceName {
			continue
		}
		out[k] = v
	}
	return out
}

func mustRenderTOML(v any) string {
	s, err := renderTOMLValue(v)
	if err != nil {
		// Only values built above reach here, and every one of them renders.
		panic(err)
	}
	return s
}

type codexExporterShape struct {
	// Kind is none, statsig, otlp-http, otlp-grpc, "" when absent, or "unknown".
	Kind     string
	Endpoint string
	Headers  map[string]any
}

func codexExporterOf(v any) codexExporterShape {
	switch x := v.(type) {
	case nil:
		return codexExporterShape{}
	case string:
		switch x {
		case codexExporterNone, codexExporterStatsig:
			return codexExporterShape{Kind: x}
		}
	case map[string]any:
		if len(x) == 1 {
			for kind, inner := range x {
				if kind != codexExporterOTLPHTTP && kind != codexExporterOTLPGRPC {
					break
				}
				fields, _ := inner.(map[string]any)
				endpoint, _ := fields[codexEndpointKey].(string)
				headers, _ := fields[codexHeadersKey].(map[string]any)
				return codexExporterShape{Kind: kind, Endpoint: endpoint, Headers: headers}
			}
		}
	}
	return codexExporterShape{Kind: "unknown"}
}

// codexBaseEndpoint strips the /v1/<signal> suffix; an endpoint without it is returned
// as-is, so status says where it points.
func codexBaseEndpoint(endpoint string, s harness.Signal) string {
	return strings.TrimSuffix(endpoint, "/v1/"+string(s))
}

// codexTermaExporter reports otlp-http at exactly the signal URL; the key is not
// compared, since a reconnect with a new key is the same destination.
func codexTermaExporter(v any, endpoint string, s harness.Signal) bool {
	shape := codexExporterOf(v)
	return shape.Kind == codexExporterOTLPHTTP && endpoint != "" &&
		shape.Endpoint == (harness.Exporter{Endpoint: endpoint}).SignalEndpoint(s)
}

func codexKeyFromExporter(v any) string {
	shape := codexExporterOf(v)
	for name, value := range shape.Headers {
		if !strings.EqualFold(name, codexAuthorizationHeader) {
			continue
		}
		s, _ := value.(string)
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "Bearer"))
	}
	return ""
}

func codexSpanAttribute(otel map[string]any, key string) string {
	attrs, _ := otel[codexSpanAttributes].(map[string]any)
	s, _ := attrs[key].(string)
	return s
}

func codexAnalyticsDisabled(doc map[string]any) bool {
	table, _ := doc[codexAnalyticsTable].(map[string]any)
	v, ok := table[codexAnalyticsEnabledKey].(bool)
	return ok && !v
}

// canonicalOtel flattens the table for the journal, entry tables one entry each.
// An unrenderable value is an error: dropped, disconnect could not restore it.
func canonicalOtel(otel map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(otel))
	for k, v := range otel {
		if slices.Contains(codexEntryTables, k) {
			attrs, ok := v.(map[string]any)
			if !ok {
				// Codex itself would refuse this file.
				return nil, fmt.Errorf("%s.%s is %s, want a table (fix the file, then retry)",
					otelTable, k, tomlTypeName(v))
			}
			for name, value := range attrs {
				s, err := renderTOMLValue(value)
				if err != nil {
					return nil, fmt.Errorf("%s.%s.%s: %w", otelTable, k, name, err)
				}
				out[k+"/"+name] = s
			}
			continue
		}
		s, err := renderTOMLValue(v)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", otelTable, k, err)
		}
		out[k] = s
	}
	return out, nil
}

func otelFromCanonical(values map[string]string) (map[string]any, error) {
	out := make(map[string]any, len(values))
	for k, text := range values {
		v, err := parseTOMLValue(text)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", otelTable, k, err)
		}
		if table, name, ok := strings.Cut(k, "/"); ok && slices.Contains(codexEntryTables, table) {
			attrs, _ := out[table].(map[string]any)
			if attrs == nil {
				attrs = map[string]any{}
				out[table] = attrs
			}
			attrs[name] = v
			continue
		}
		out[k] = v
	}
	return out, nil
}
