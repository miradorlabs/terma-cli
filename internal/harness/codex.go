package harness

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

	"github.com/miradorlabs/terma-cli/internal/serverkey"
)

// Codex configures the Codex CLI's OpenTelemetry export.
//
// Codex does not read the OTEL_* environment variables. Its export is configured in the
// `[otel]` table of config.toml, one exporter per signal, each carrying its own endpoint
// and headers — so there is no generic endpoint for a per-signal override to leak a
// credential through, and no headers-helper mechanism to keep the key out of the file.
// The key is written inline and the file tightened to 0600. The key names are Codex's
// own contract (codex-rs/config, OtelConfigToml); they are constants here so a rename
// upstream is a one-line, compile-time-visible change.
//
// Verified against Codex 0.152. What each signal carries:
//
//   - traces: spans per turn and model request, with gen_ai.usage.* token counts;
//   - logs: codex.* events — conversation starts, API requests, codex.sse_event with
//     per-response token counts, codex.user_prompt, codex.tool_decision,
//     codex.tool_result, and codex.turn_cost with the estimated USD;
//   - metrics: counters and histograms including codex.turn.token_usage and
//     codex.turn.cost_microusd.
type Codex struct{}

const (
	// The three exporters. `exporter` is the log exporter — the name predates the other
	// two — and each defaults to "none" except metrics, which defaults to "statsig":
	// OpenAI's own analytics. Connecting the metrics signal replaces that.
	codexLogExporter     = "exporter"
	codexTraceExporter   = "trace_exporter"
	codexMetricsExporter = "metrics_exporter"

	// codexLogUserPrompt is the one content switch. Off upstream; with it off the
	// codex.user_prompt event carries "[REDACTED]" and the prompt's length. Codex never
	// exports model response text, so this is the whole of the prompts posture.
	codexLogUserPrompt = "log_user_prompt"

	// codexToolResult caps the bytes of tool output on the codex.tool_result event
	// (2048 upstream). Zero drops the output. Tool *arguments* — the command that ran —
	// are logged regardless; Codex has no switch for those.
	codexToolResult         = "tool_result"
	codexToolResultMaxBytes = "max_bytes"

	// codexSpanAttributes are stamped on every span, and only spans. It is the only
	// per-user attribute Codex accepts: resource attributes are not configurable, so
	// logs and metrics carry Codex's own service.name and env and nothing of Terma's.
	//
	// The table may hold the user's own attributes too, so Terma owns entries in it,
	// never the table. In the flat view the journal works on, an entry is keyed
	// `span_attributes/<attribute>`; the slash cannot appear in an otel key, so the
	// first one splits the path unambiguously even for attributes that contain dots.
	codexSpanAttributes      = "span_attributes"
	codexSpanAttributePrefix = codexSpanAttributes + "/"

	// The analytics opt-out is its own table, not an otel key. When `enabled` is false
	// Codex installs no metrics exporter at all, whatever `metrics_exporter` says — the
	// one way a metrics connect can look right and send nothing.
	codexAnalyticsTable      = "analytics"
	codexAnalyticsEnabledKey = "enabled"

	codexExporterNone     = "none"
	codexExporterStatsig  = "statsig"
	codexExporterOTLPHTTP = "otlp-http"
	codexExporterOTLPGRPC = "otlp-grpc"

	codexEndpointKey = "endpoint"
	codexHeadersKey  = "headers"
	codexProtocolKey = "protocol"
	// codexProtocolBinary is http/protobuf in Codex's spelling; json is the other.
	codexProtocolBinary = "binary"

	codexAuthorizationHeader = "Authorization"

	// codexServiceName is the originator the codex CLI stamps as service.name.
	codexServiceName = "codex_cli_rs"

	codexConfigFile = "config.toml"
	// codexProfileSuffix names the profile files under CODEX_HOME: `<name>.config.toml`,
	// layered over config.toml while that profile is selected with --profile.
	codexProfileSuffix = ".config.toml"

	// The administrator-managed layers, which outrank everything the user writes: a
	// managed_config.toml (see codexManagedConflicts for where), and on macOS the
	// managed preference an MDM profile delivers, a base64-encoded config.toml.
	codexManagedConfigUnix       = "/etc/codex/managed_config.toml"
	codexManagedConfigWindows    = "managed_config.toml"
	codexManagedPreferenceDomain = "com.openai.codex"
	codexManagedPreferenceKey    = "config_toml_base64"
)

// codexSignalKeys maps each signal to its exporter key, in report order.
var codexSignalKeys = []struct {
	signal Signal
	key    string
}{
	{SignalTraces, codexTraceExporter},
	{SignalLogs, codexLogExporter},
	{SignalMetrics, codexMetricsExporter},
}

// Name is the token `terma connect` and `--harness` accept.
func (Codex) Name() string { return "codex" }

// DisplayName is how the agent is written in prose.
func (Codex) DisplayName() string { return "Codex" }

// SupportsHeadersHelper is false: Codex's headers are literal strings in config.toml
// and nothing else, so the key is written inline and the file's mode tightened.
func (Codex) SupportsHeadersHelper() bool { return false }

// Detect runs `codex --version`. A missing binary is not-found rather than an error.
func (Codex) Detect(ctx context.Context) Detection {
	return detectBinary(ctx, "codex", semverRE)
}

// codexHome is $CODEX_HOME, or ~/.codex. Honouring the variable matters for the same
// reason CLAUDE_CONFIG_DIR does: writing where Codex does not read is a connect that
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
func (Codex) ConfigPath() (string, error) {
	dir, err := codexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, codexConfigFile), nil
}

// Render maps an Exporter onto the otel table. Values are TOML text — the canonical
// rendering of each value, which is also the form the ownership journal records. Span
// attributes are rendered one entry at a time, as `span_attributes/<attribute>`, so a
// connect adds Terma's two to whatever the table already holds.
//
// Only the exporters for the selected signals are written. Codex's exporters are
// self-contained, so an unselected signal is simply left as it was: for logs and traces
// that is off, and for metrics it is OpenAI's own statsig route, which is not Terma's
// to switch off on the way past.
func (Codex) render(e Exporter) map[string]string {
	out := map[string]string{
		codexLogUserPrompt: mustRenderTOML(e.IncludePrompts),
	}
	for _, sk := range codexSignalKeys {
		if e.HasSignal(sk.signal) {
			out[sk.key] = mustRenderTOML(codexOTLPExporter(e, sk.signal))
		}
	}
	// Written only when excluding: with content on, Codex's own cap (or one the user
	// chose) stays in force. A stale zero from an earlier exclude is undone by Connect.
	if !e.IncludeToolContent {
		out[codexToolResult] = mustRenderTOML(map[string]any{codexToolResultMaxBytes: int64(0)})
	}
	for key, value := range codexSpanAttributeValues(e.ResourceAttributes) {
		out[codexSpanAttributePrefix+key] = mustRenderTOML(value)
	}
	return out
}

// RuntimeArgs configures this launch without changing Codex's home or persisted
// settings. Authorization is deliberately passed in argv, just like the exporter
// endpoint; callers must never log these arguments.
func (c Codex) RuntimeArgs(e Exporter) []string {
	values := c.render(e)
	// Attribute names contain dots. Codex splits override paths on every dot,
	// without TOML key quoting, so carry attributes inside the inline table value.
	for k := range values {
		if strings.HasPrefix(k, codexSpanAttributePrefix) {
			delete(values, k)
		}
	}
	if attrs := codexSpanAttributeValues(e.ResourceAttributes); len(attrs) > 0 {
		values[codexSpanAttributes] = mustRenderTOML(attrs)
	}
	// Per-repo -c overrides must be authoritative for every signal, not just the selected
	// ones. A prior machine-wide `connect codex` may have left another project's exporters
	// in the user-level config.toml; Codex would fall through to them for any signal this
	// repo does not override, exporting this repo's telemetry to that other project. Turn
	// the unselected signals off explicitly so a per-repo run sends exactly this project's
	// chosen signals and nothing else.
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
	if e.HasSignal(SignalMetrics) {
		args = append(args, "-c", "analytics.enabled=true")
	}
	return args
}

// codexOTLPExporter is the exporter value for one signal: otlp-http, binary protocol
// (which traverses ordinary HTTPS proxies, as with Claude), the signal-specific URL —
// Codex uses an exporter endpoint as-is, so it must carry /v1/<signal> — and the
// Authorization header inline.
func codexOTLPExporter(e Exporter, s Signal) map[string]any {
	inner := map[string]any{
		codexEndpointKey: e.SignalEndpoint(s),
		codexProtocolKey: codexProtocolBinary,
	}
	if e.APIKey != "" {
		inner[codexHeadersKey] = map[string]any{codexAuthorizationHeader: "Bearer " + e.APIKey}
	}
	return map[string]any{codexExporterOTLPHTTP: inner}
}

// codexSpanAttributeValues is the resource attributes minus service.name: Codex stamps
// its own on the resource, and repeating it as a span attribute would only confuse.
func codexSpanAttributeValues(attrs map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range attrs {
		if k == "" || v == "" || k == AttrServiceName {
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

// codexExporterShape is what a parsed exporter value says about itself.
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

// codexBaseEndpoint strips the /v1/<signal> suffix Codex needs on an exporter endpoint,
// giving back the base URL a Terma profile is configured with. An endpoint without
// the suffix is returned as-is: it is misconfigured, and status should say where it
// points rather than hide it.
func codexBaseEndpoint(endpoint string, s Signal) string {
	return strings.TrimSuffix(endpoint, "/v1/"+string(s))
}

// codexTermaExporter reports whether v is an exporter Terma would have written for
// endpoint and signal — otlp-http at exactly the signal URL. The key is not compared:
// a reconnect with a different key is still the same destination.
func codexTermaExporter(v any, endpoint string, s Signal) bool {
	shape := codexExporterOf(v)
	return shape.Kind == codexExporterOTLPHTTP && endpoint != "" &&
		shape.Endpoint == (Exporter{Endpoint: endpoint}).SignalEndpoint(s)
}

// codexKeyFromExporter extracts the bearer token from an exporter's headers, or "".
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

// codexToolContentOn reads the tool_result cap: absent means Codex's default, which
// sends output; only an explicit zero drops it.
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

// canonicalOtel renders the table as the flat view the journal works on: each key as
// TOML text, except span_attributes, whose entries appear one by one under
// `span_attributes/<attribute>` so that ownership can stop at the entry. A value that
// cannot be rendered is reported rather than dropped: silently losing it would make
// disconnect unable to restore it.
func canonicalOtel(otel map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(otel))
	for k, v := range otel {
		if k == codexSpanAttributes {
			attrs, ok := v.(map[string]any)
			if !ok {
				// Codex itself would refuse this file; say so rather than fold a
				// scalar into a table it never was.
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

// otelFromCanonical is the inverse of canonicalOtel. A span_attributes table with no
// entries left is omitted rather than written empty.
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
func (c Codex) Status() (Status, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return Status{}, err
	}
	f, err := loadTOML(path)
	if err != nil {
		return Status{}, err
	}

	status := Status{
		ConfigPath:         path,
		Exists:             f.existed,
		IncludePrompts:     f.otel[codexLogUserPrompt] == true,
		IncludeToolContent: codexToolContentOn(f.otel),
		ProjectID:          codexSpanAttribute(f.otel, AttrProjectID),
	}

	// The endpoint is whichever OTLP destination the first configured signal uses, and
	// the signals are those sharing it. Codex has no on/off switch beyond the exporters
	// themselves, so an OTLP exporter present is telemetry on.
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
			// Only a Terma key is worth naming. The exporter may carry somebody
			// else's bearer token, and even its head has no business in a status line.
			if key := codexKeyFromExporter(f.otel[sk.key]); status.KeyPrefix == "" && serverkey.Is(key) {
				status.KeyPrefix = MaskKey(key)
			}
		}
	}
	status.Connected = status.Endpoint != ""

	// Compared against the endpoint actually configured here, so status reports whether
	// this file is internally consistent. Computed before the analytics check below
	// drops metrics, so that check is what gets reported.
	status.Conflicts = codexConflicts(f, Exporter{
		Endpoint:           status.Endpoint,
		Signals:            status.Signals,
		IncludePrompts:     status.IncludePrompts,
		IncludeToolContent: status.IncludeToolContent,
		ResourceAttributes: map[string]string{
			AttrEnduserID: codexSpanAttribute(f.otel, AttrEnduserID),
			AttrProjectID: status.ProjectID,
		},
	})
	// A metrics exporter with analytics disabled is set and sends nothing. Reporting
	// metrics as on would send someone hunting in Terma for data Codex never sent.
	if codexAnalyticsDisabled(f.doc) {
		status.Signals = withoutSignal(status.Signals, SignalMetrics)
	}

	// Ownership is by journal only. There is no name-based fallback here, unlike
	// Claude's: no released build ever wrote a Codex config without a journal, so a
	// config with none is somebody else's work — a company collector, say — and every
	// standard otel key in it is theirs. Counting those as managed would let disconnect
	// delete a telemetry setup Terma never touched.
	j, err := loadJournal(c.Name(), path)
	if err != nil {
		return Status{}, err
	}
	if j != nil {
		current, err := canonicalOtel(f.otel)
		if err != nil {
			return Status{}, err
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
func (c Codex) ConflictsWith(e Exporter) ([]Conflict, error) {
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

// codexConflicts finds, for each signal e exports: an exporter already pointing
// somewhere else (replaced by the connect, so it needs consent); the analytics opt-out
// that silences metrics regardless of the exporter; and the same keys in files that
// outrank the user config, which Terma cannot change.
//
// There is no credential-disclosure case here. Each Codex exporter carries its own
// headers, so nothing Terma writes for one signal can be inherited by another
// destination.
func codexConflicts(f *tomlFile, e Exporter) []Conflict {
	var out []Conflict

	for _, sk := range codexSignalKeys {
		if !e.HasSignal(sk.signal) {
			continue
		}
		shape := codexExporterOf(f.otel[sk.key])
		key := otelTable + "." + sk.key
		switch shape.Kind {
		case "", codexExporterNone, codexExporterStatsig:
			// Off, or Codex's own default route. Nothing of the user's is being replaced.
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
			out = append(out, Conflict{
				Key: key, Value: shape.Endpoint, Reason: reason,
				Scope: ScopeUserSettings, Clearable: true,
			})
		case codexExporterOTLPGRPC:
			out = append(out, Conflict{
				Key:    key,
				Value:  shape.Endpoint,
				Reason: "already exports " + string(sk.signal) + " here over gRPC; connecting replaces it",
				Scope:  ScopeUserSettings, Clearable: true,
			})
		default:
			out = append(out, Conflict{
				Key:    key,
				Reason: "is set to something Terma does not recognize as an exporter; connecting replaces it",
				Scope:  ScopeUserSettings, Clearable: true,
			})
		}
	}

	// Not an otel key, and not Terma's to flip: it is the user's opt-out from OpenAI's
	// analytics as well as the metrics switch. Refuse the metrics signal instead.
	if e.HasSignal(SignalMetrics) && codexAnalyticsDisabled(f.doc) {
		out = append(out, Conflict{
			Key:   codexAnalyticsTable + "." + codexAnalyticsEnabledKey,
			Value: "false",
			Reason: "Codex sends no metrics at all while analytics are disabled, so the metrics signal " +
				"would connect and deliver nothing — remove this setting, or connect with --signals traces,logs",
			Scope:     ScopeUserSettings,
			Clearable: false,
		})
	}

	out = append(out, codexManagedConflicts(e)...)
	out = append(out, codexProfileConflicts(e)...)
	return out
}

// codexLayer is one configuration layer above the user file, for reporting.
type codexLayer struct {
	// source qualifies each conflict key — a path, a profile file name, or the managed
	// preference id — so a status report cannot mistake it for the user config.
	source string
	scope  string
	// where opens every reason: what the layer is and when Codex applies it.
	where string
	// advisory is set for a layer that applies only when the user selects it.
	advisory bool
}

// codexManagedConflicts reports settings an administrator has placed above the user
// config: managed_config.toml, and on macOS the managed preference an MDM profile
// delivers. A project's .codex/config.toml is deliberately not scanned: Codex refuses
// `otel` from project-local config, so a setting there is inert and must not block.
//
// On macOS and Linux the file is /etc/codex/managed_config.toml. On Windows it is
// $CODEX_HOME/managed_config.toml, which Codex 0.150 and later ignore with a startup
// warning while earlier builds apply above the user config. It is read regardless: a
// file that is there is either live or a leftover Codex itself complains about, and
// neither is Terma's to overlook.
func codexManagedConflicts(e Exporter) []Conflict {
	var out []Conflict
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
			scope:  ScopeManaged,
			where:  "set in " + path + ", which Codex applies over your user config",
		}, e)...)
	}
	if runtime.GOOS == "darwin" {
		if data, ok := codexManagedPreference(); ok {
			source := codexManagedPreferenceDomain + ":" + codexManagedPreferenceKey
			out = append(out, codexConflictsInLayer(data, codexLayer{
				source: source,
				scope:  ScopeManaged,
				where:  "set by the managed preference " + source + ", which Codex applies over your user config",
			}, e)...)
		}
	}
	return out
}

// codexManagedPreference reads the MDM-delivered config through `defaults`, which
// consults the same managed-preferences domain Codex does. Absent is the ordinary case.
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

// codexProfileConflicts reports settings in profile files — `<name>.config.toml` beside
// config.toml — which Codex layers over the user config only while that profile is
// selected with --profile. Which profile a session will use is not knowable here, so
// every profile is scanned and each finding is advisory: named, so the user can decide,
// but not a reason to refuse the connect that the plain configuration asked for.
func codexProfileConflicts(e Exporter) []Conflict {
	home, err := codexHome()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil
	}
	var out []Conflict
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
			scope:    ScopeProfile,
			where:    "set in " + path + ", which Codex applies over your user config only while that profile is selected with --profile " + profile,
			advisory: true,
		}, e)...)
	}
	return out
}

// codexConflictsInLayer reports every setting in a higher-precedence layer that would
// change what the export e describes: an exporter for a selected signal (any value,
// including an explicit "none" — the layer decides the destination, and the user-level
// exporter is never consulted), the two content switches when they contradict the
// posture Terma is writing, an attribution attribute set to something else, and the
// analytics opt-out that silences metrics. Codex merges layers table by table, so each
// of these overrides exactly the key it names.
func codexConflictsInLayer(data []byte, layer codexLayer, e Exporter) []Conflict {
	var doc struct {
		Otel      map[string]any `toml:"otel"`
		Analytics map[string]any `toml:"analytics"`
	}
	if unmarshalTOMLLenient(data, &doc) != nil {
		// A file this CLI cannot parse is not this CLI's to complain about; Codex will
		// report its own parse error.
		return nil
	}

	var out []Conflict
	report := func(key, value, what string) {
		out = append(out, Conflict{
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
		for _, key := range []string{AttrEnduserID, AttrProjectID} {
			v, ok := attrs[key].(string)
			if !ok || v == e.ResourceAttributes[key] {
				continue
			}
			report(otelTable+"."+codexSpanAttributes+"."+key, v, "changes the "+key+" stamped on spans")
		}
	}
	if e.HasSignal(SignalMetrics) {
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

// Connect merges Terma's keys into the otel table, leaving every other key — in the
// table and in the file — as it was.
//
// A key an earlier connect installed that this one does not write (the metrics exporter
// on a reconnect with fewer signals, the tool-result cap once content is back on) is
// put back to its pre-Terma value here, provided it still holds what Terma wrote.
// Otherwise a reconnect that asked for less would silently keep sending more.
func (c Codex) Connect(e Exporter, clearConflicts bool) error {
	rendered := c.render(e)

	path, err := c.ConfigPath()
	if err != nil {
		return err
	}
	f, err := loadTOML(path)
	if err != nil {
		return err
	}
	previousJournal, err := loadJournal(c.Name(), path)
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
			// About to be overwritten by the merge anyway; the journal records the
			// prior value as Previous, and disconnect restores it from there.
			if _, overwritten := rendered[key]; overwritten {
				continue
			}
			if value, ok := current[key]; ok {
				cleared[key] = value
			}
			delete(current, key)
		}
	}

	// carried is the earlier record minus the keys restored here, so the new journal
	// does not inherit ownership of a key this connect just gave back. previousJournal
	// itself is kept intact for the rollback below.
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

	j := newJournal(c.Name(), path, current, rendered, cleared, nil, carried)

	maps.Copy(current, rendered)
	f.otel, err = otelFromCanonical(current)
	if err != nil {
		return err
	}

	// Ownership first, then the file, with the same rollback as Claude's connect.
	if err := j.save(); err != nil {
		return err
	}
	defer pruneJournals()
	if err := f.save(e.APIKey != ""); err != nil {
		var rollbackErr error
		if previousJournal != nil {
			rollbackErr = previousJournal.save()
		} else {
			rollbackErr = deleteJournal(c.Name(), path)
		}
		if rollbackErr != nil {
			return fmt.Errorf("write config: %w (also failed to restore telemetry journal: %w)", err, rollbackErr)
		}
		return err
	}
	return nil
}

// Disconnect undoes the recorded connect, or without a record removes the managed keys.
func (c Codex) Disconnect() (DisconnectResult, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return DisconnectResult{}, err
	}
	f, err := loadTOML(path)
	if err != nil {
		return DisconnectResult{}, err
	}
	j, err := loadJournal(c.Name(), path)
	if err != nil {
		return DisconnectResult{}, err
	}
	current, err := canonicalOtel(f.otel)
	if err != nil {
		return DisconnectResult{}, err
	}

	// Without a journal there is nothing here that Terma wrote — see Status — so
	// there is nothing to undo, and certainly not a foreign telemetry setup to delete.
	if j == nil {
		return DisconnectResult{}, nil
	}
	result, remaining := j.apply(current)
	sort.Strings(result.Skipped)

	defer pruneJournals()
	if result.Removed > 0 || result.Restored > 0 {
		f.otel, err = otelFromCanonical(current)
		if err != nil {
			return result, err
		}
		if err := f.save(false); err != nil {
			return result, err
		}
	}

	if remaining != nil && !remaining.empty() {
		return result, remaining.save()
	}
	return result, deleteJournal(c.Name(), path)
}

// Backup exposes the pre-modification copy. Same rule as Claude's: a journal for this
// file means its contents are Terma's work and the stored backup is kept; without
// one, whether the config points at endpoint is the only evidence.
func (c Codex) Backup(endpoint string) (string, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return "", err
	}
	f, err := loadTOML(path)
	if err != nil {
		return "", err
	}
	j, err := loadJournal(c.Name(), path)
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

// ConnectNotes are the two things a Codex connect does, or cannot do, that the plan's
// generic lines do not say: connecting metrics takes them away from OpenAI's own
// route, and excluding tool content cannot exclude the tool's arguments.
func (Codex) ConnectNotes(e Exporter) []string {
	var notes []string
	if e.HasSignal(SignalMetrics) {
		notes = append(notes, "Codex sends metrics to OpenAI (statsig) unless configured otherwise; after this connect they go to Terma instead.")
	}
	if !e.IncludeToolContent {
		notes = append(notes, "Codex has no switch for tool arguments: tool output is dropped, but the command or parameters of each tool call are still exported.")
	}
	return notes
}

// CurrentCredential returns the key already installed for endpoint and projectID, so a
// reconnect reuses it instead of minting an orphan. Both must match, for the reasons
// given on Claude's.
func (c Codex) CurrentCredential(endpoint, projectID string) (string, bool) {
	path, err := c.ConfigPath()
	if err != nil {
		return "", false
	}
	f, err := loadTOML(path)
	if err != nil {
		return "", false
	}
	if codexSpanAttribute(f.otel, AttrProjectID) != projectID {
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
