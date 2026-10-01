package codex

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

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
func (exporter) Name() string { return "codex" }

// ServiceName is codex_cli_rs, the originator Codex stamps for its CLI; Desktop and the
// IDE extensions report their own.
func (exporter) ServiceName() string { return codexServiceName }

// DisplayName is how the agent is written in prose.
func (exporter) DisplayName() string { return "Codex" }

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

// Render maps an Exporter onto the otel table as TOML text, span attributes one entry at
// a time; an unselected signal is left as it was (for metrics, OpenAI's own route).
func (exporter) render(e harness.Exporter) map[string]string {
	out := map[string]string{
		codexLogUserPrompt: mustRenderTOML(e.IncludePrompts),
	}
	for _, sk := range codexSignalKeys {
		if e.HasSignal(sk.signal) {
			out[sk.key] = mustRenderTOML(codexOTLPExporter(e, sk.signal))
		}
	}
	// Written only when excluding, so Codex's or the user's own cap otherwise stays.
	if !e.IncludeToolContent {
		out[codexToolResult] = mustRenderTOML(map[string]any{codexToolResultMaxBytes: int64(0)})
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
	// Codex splits override paths on every dot without TOML quoting, so dotted attribute
	// names travel inside the inline table value.
	for k := range values {
		if strings.HasPrefix(k, codexSpanAttributePrefix) {
			delete(values, k)
		}
	}
	if attrs := codexSpanAttributeValues(e.ResourceAttributes); len(attrs) > 0 {
		values[codexSpanAttributes] = mustRenderTOML(attrs)
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
	sort.Strings(keys)
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

// codexToolContentOn: absent means Codex's default, which sends output.
func codexToolContentOn(otel map[string]any) bool {
	table, ok := otel[codexToolResult].(map[string]any)
	if !ok {
		return true
	}
	switch n := table[codexToolResultMaxBytes].(type) {
	case int64:
		return n > 0
	case float64:
		return n > 0
	default:
		return true
	}
}

func codexAnalyticsDisabled(doc map[string]any) bool {
	table, _ := doc[codexAnalyticsTable].(map[string]any)
	v, ok := table[codexAnalyticsEnabledKey].(bool)
	return ok && !v
}

// canonicalOtel flattens the table for the journal, span attributes one entry each.
// An unrenderable value is an error: dropped, disconnect could not restore it.
func canonicalOtel(otel map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(otel))
	for k, v := range otel {
		if k == codexSpanAttributes {
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
				out[codexSpanAttributePrefix+name] = s
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
		if name, ok := strings.CutPrefix(k, codexSpanAttributePrefix); ok {
			attrs, _ := out[codexSpanAttributes].(map[string]any)
			if attrs == nil {
				attrs = map[string]any{}
				out[codexSpanAttributes] = attrs
			}
			attrs[name] = v
			continue
		}
		out[k] = v
	}
	return out, nil
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

	if v, ok := doc.Otel[codexLogUserPrompt].(bool); ok && v != e.IncludePrompts {
		report(otelTable+"."+codexLogUserPrompt, strconv.FormatBool(v), "turns prompt capture "+switchWord(v))
	}
	if table, ok := doc.Otel[codexToolResult].(map[string]any); ok {
		if n, ok := tomlInt(table[codexToolResultMaxBytes]); ok && (n > 0) != e.IncludeToolContent {
			report(otelTable+"."+codexToolResult+"."+codexToolResultMaxBytes, strconv.FormatInt(n, 10),
				"turns tool output capture "+switchWord(n > 0))
		}
	}
	if attrs, ok := doc.Otel[codexSpanAttributes].(map[string]any); ok {
		for _, key := range []string{harness.AttrEnduserID, harness.AttrProjectID} {
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
	sort.Strings(result.Skipped)

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
var (
	_ harness.Harness      = exporter{}
	_ harness.Noter        = exporter{}
	_ harness.Credentialed = exporter{}
	_ harness.Backuper     = exporter{}
)
