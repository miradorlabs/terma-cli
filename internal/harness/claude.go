package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/serverkey"
)

// Claude configures Claude Code's OpenTelemetry export.
//
// Claude Code reads environment variables out of the `env` object in its settings file,
// which is what makes this installable without touching the user's shell profile. The
// variable names are Claude Code's own contract, documented under "Monitoring usage";
// they are spelled out as constants here so a rename upstream is a one-line change and
// a compile-time-visible one.
//
// The zero value is the global harness, acting on the user-level settings file. Local
// binds it to a repository instead (see Scope), where it writes the project settings
// file and only the keys a repository may decide.
type Claude struct {
	// root, when set, is the repository whose .claude/settings.json this value acts on.
	root string
}

// claudeProjectSettings is the project settings file, relative to a repository root —
// the same file `terma install` puts the session hooks in. Claude Code applies it over
// the user file, which is what lets a repository narrow what its sessions ship.
const claudeProjectSettings = ".claude/settings.json"

// claudeProjectLocalSettings is the per-developer, gitignored project file, applied
// over claudeProjectSettings. Terma never writes it; it is scanned as an outranking scope.
const claudeProjectLocalSettings = ".claude/settings.local.json"

// Local returns the harness bound to the repository at root.
func (Claude) Local(root string) Harness { return Claude{root: root} }

// Scope reports which layer this value acts on.
func (c Claude) Scope() Scope {
	if c.root != "" {
		return ScopeLocal
	}
	return ScopeGlobal
}

const (
	// claudeEnableTelemetry is the master switch. Without it every OTEL_* variable
	// below is inert.
	claudeEnableTelemetry = "CLAUDE_CODE_ENABLE_TELEMETRY"
	// claudeEnhancedTelemetry gates spans specifically. OTEL_TRACES_EXPORTER alone
	// produces nothing without it, which is the single easiest way to end up
	// "connected" and see no traces.
	claudeEnhancedTelemetry = "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA"

	otelTracesExporter  = "OTEL_TRACES_EXPORTER"
	otelLogsExporter    = "OTEL_LOGS_EXPORTER"
	otelMetricsExporter = "OTEL_METRICS_EXPORTER"

	otelProtocol           = "OTEL_EXPORTER_OTLP_PROTOCOL"
	otelEndpoint           = "OTEL_EXPORTER_OTLP_ENDPOINT"
	otelHeaders            = "OTEL_EXPORTER_OTLP_HEADERS"
	otelResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"

	// The four content switches. All default off upstream; Terma writes them
	// explicitly either way so the file states the redaction posture rather than
	// leaving it to a default that could change.
	otelLogUserPrompts       = "OTEL_LOG_USER_PROMPTS"
	otelLogAssistantResponse = "OTEL_LOG_ASSISTANT_RESPONSES"
	otelLogToolDetails       = "OTEL_LOG_TOOL_DETAILS"
	otelLogToolContent       = "OTEL_LOG_TOOL_CONTENT"

	// exporterOTLP / exporterNone are the only two values Terma writes. A disabled
	// signal is written as an explicit "none" rather than omitted, so the config says
	// what it does instead of depending on an upstream default.
	exporterOTLP = "otlp"
	exporterNone = "none"

	// protocolHTTPProtobuf is chosen over grpc because it traverses ordinary HTTPS
	// proxies and corporate TLS interception, which the gRPC transport frequently
	// does not.
	protocolHTTPProtobuf = "http/protobuf"

	// The detailed-beta-tracing pair. Together these send logs and traces to
	// BETA_TRACING_ENDPOINT *instead of* through the configured exporters — a redirect
	// that bypasses OTEL_EXPORTER_OTLP_ENDPOINT entirely, so checking only the OTLP
	// variables would miss it.
	//
	// Anthropic treats this as part of the same boundary: managed settings that pin an
	// endpoint or credential strip a developer-set BETA_TRACING_ENDPOINT. Terma writes
	// user settings, not managed settings, so it gets none of that protection and has to
	// do the check itself.
	claudeBetaTracingDetailed = "ENABLE_BETA_TRACING_DETAILED"
	claudeBetaTracingEndpoint = "BETA_TRACING_ENDPOINT"

	// claudeOtelHeadersHelper names a script that generates OTLP headers dynamically.
	// It is a *top-level* setting rather than an env entry, so a scan of the `env` block
	// never sees it — and it supplies the same Authorization header Terma writes,
	// which means an existing helper decides what the export authenticates with while
	// Terma reports itself connected.
	claudeOtelHeadersHelper = "otelHeadersHelper"
)

// perSignalOverrides are the variables that take precedence over the generic ones
// Terma writes. Claude Code resolves each signal's exporter from the per-signal value
// when present, and *merges* the generic headers into it — so a stale
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT pointing at another vendor keeps receiving that
// signal's spans, now carrying Terma's Authorization header. That is a live server key
// handed to a third party, and nothing downstream would show it: `telemetry status` reads
// the generic endpoint and would report Terma.
//
// Terma never writes these. They are detected before a connect and reported, and only
// removed when the user explicitly asks — they are the user's settings.
var perSignalOverrides = []struct {
	endpoint, protocol, headers string
	signal                      Signal
}{
	{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", SignalTraces},
	{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", SignalLogs},
	{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", SignalMetrics},
}

// claudeManagedKeys is every variable Terma sets, and therefore exactly what
// disconnect removes. A key absent from this list would be orphaned in the user's
// config forever; a key wrongly present would delete a setting Terma never made.
//
// The per-signal overrides above are deliberately absent: Terma does not set them, so
// disconnect must not delete them.
var claudeManagedKeys = []string{
	claudeEnableTelemetry,
	claudeEnhancedTelemetry,
	otelTracesExporter,
	otelLogsExporter,
	otelMetricsExporter,
	otelProtocol,
	otelEndpoint,
	otelHeaders,
	otelLogUserPrompts,
	otelLogAssistantResponse,
	otelLogToolDetails,
	otelLogToolContent,
}

// claudeLocalKeys are the variables a repository-scope connect writes: what to ship.
// Everything that decides where it goes and how it authenticates — the master switch,
// endpoint, protocol, headers, and the identity attributes — stays in the user file, so
// a committed project file never carries a destination or a credential. The beta switch
// is here because traces need it and the repository may turn traces on where the global
// connect left them off.
//
// This is also exactly what a local disconnect removes and what a local connect may
// clear: a project file's own OTEL_EXPORTER_OTLP_ENDPOINT is somebody's setting, and
// the local layer has no business deleting it because its render happens not to
// include it.
var claudeLocalKeys = []string{
	claudeEnhancedTelemetry,
	otelTracesExporter,
	otelLogsExporter,
	otelMetricsExporter,
	otelLogUserPrompts,
	otelLogAssistantResponse,
	otelLogToolDetails,
	otelLogToolContent,
}

// claudeRetiredKeys were written by earlier versions of Terma and never are again.
// OTEL_RESOURCE_ATTRIBUTES carried the identity, the project and a service.name; it is
// the user's own variable for describing their resources, and nothing Terma put in it
// is needed: the server key names the project, Claude Code stamps user.id and
// user.email on every metric and event itself, and its resource already says
// service.name=claude-code.
//
// A connect clears a retired key only while the journal shows it still holds what an
// earlier Terma installed — a value the user set, or edited, is theirs. The paths with
// no journal to consult (a legacy disconnect, a legacy status count) treat retired keys
// as managed, because an install old enough to have no journal did write them.
var claudeRetiredKeys = []string{otelResourceAttributes}

// managedKeys is what this value owns in the file it writes.
func (c Claude) managedKeys() []string {
	if c.root != "" {
		return claudeLocalKeys
	}
	return claudeManagedKeys
}

// retiredKeys is what earlier versions wrote into the file this value writes. The
// repository scope never carried them.
func (c Claude) retiredKeys() []string {
	if c.root != "" {
		return nil
	}
	return claudeRetiredKeys
}

// legacyKeys is every key an install without a journal may owe to Terma.
func (c Claude) legacyKeys() []string {
	return append(append([]string{}, c.managedKeys()...), c.retiredKeys()...)
}

// Name is the token `terma connect` and `--harness` accept.
func (Claude) Name() string { return "claude" }

// DisplayName is how the agent is written in prose.
func (Claude) DisplayName() string { return "Claude Code" }

// SupportsHeadersHelper is true: Claude Code has the otelHeadersHelper setting, see
// headers_helper.go.
func (Claude) SupportsHeadersHelper() bool { return true }

// Detect runs `claude --version`. A missing binary is reported as not-found rather than
// as an error: connecting an uninstalled harness is allowed, since the config is read
// whenever it is eventually started.
func (Claude) Detect(ctx context.Context) Detection {
	return detectBinary(ctx, "claude", semverRE)
}

// ConfigPath is ~/.claude/settings.json, or $CLAUDE_CONFIG_DIR/settings.json when Claude
// Code has been pointed elsewhere. Honouring that variable matters: writing to the
// default path while Claude reads another is a connect that silently does nothing.
//
// Bound to a repository, it is that repository's .claude/settings.json, which
// CLAUDE_CONFIG_DIR does not move.
func (c Claude) ConfigPath() (string, error) {
	if c.root != "" {
		return filepath.Join(c.root, filepath.FromSlash(claudeProjectSettings)), nil
	}
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// Render maps an Exporter onto Claude Code's variables.
//
// At repository scope the result is cut down to claudeLocalKeys: the endpoint, key,
// protocol, identity and master switch in e are ignored, because a project file must
// not carry them. What remains still states every switch explicitly, so the file reads
// as a complete policy rather than a diff against a global file the reader cannot see.
func (c Claude) render(e Exporter) map[string]string {
	env := renderClaude(e)
	if c.root == "" {
		return env
	}
	for key := range env {
		if !slices.Contains(claudeLocalKeys, key) {
			delete(env, key)
		}
	}
	return env
}

func renderClaude(e Exporter) map[string]string {
	traces := e.HasSignal(SignalTraces)

	env := map[string]string{
		claudeEnableTelemetry: "1",

		otelTracesExporter:  exporterFor(traces),
		otelLogsExporter:    exporterFor(e.HasSignal(SignalLogs)),
		otelMetricsExporter: exporterFor(e.HasSignal(SignalMetrics)),

		otelProtocol: protocolHTTPProtobuf,
		otelEndpoint: e.Endpoint,

		// Off unless explicitly opted into. Written rather than omitted so the file is
		// an explicit statement of what is and is not captured.
		otelLogUserPrompts:       boolValue(e.IncludePrompts),
		otelLogAssistantResponse: boolValue(e.IncludePrompts),
		otelLogToolDetails:       boolValue(e.IncludeToolContent),
		otelLogToolContent:       boolValue(e.IncludeToolContent),
	}

	// Spans are gated behind the beta flag as well as the exporter. Setting it only
	// when traces are on keeps a logs-and-metrics connect from opting the user into a
	// beta they did not ask for.
	if traces {
		env[claudeEnhancedTelemetry] = "1"
	}

	// In helper mode the credential travels through the headers-helper script instead;
	// writing it here too would defeat the point of keeping it out of the settings file.
	if e.APIKey != "" && e.HelperPath == "" {
		env[otelHeaders] = "Authorization=Bearer " + e.APIKey
	}
	// e.ResourceAttributes are deliberately not rendered. OTEL_RESOURCE_ATTRIBUTES
	// belongs to the user (see claudeRetiredKeys); the project travels in the connect
	// journal instead, and Claude Code supplies the identity and service name itself.
	return env
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

// Status reads back what is currently installed.
func (c Claude) Status() (Status, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return Status{}, err
	}
	s, err := loadSettings(path)
	if err != nil {
		return Status{}, err
	}

	status := Status{
		ConfigPath: path,
		Exists:     s.existed,
		Endpoint:   s.env[otelEndpoint],

		// Absent is off — matching Claude Code's own default rather than reporting a
		// missing key as unknown.
		IncludePrompts:     isOn(s.env[otelLogUserPrompts]),
		IncludeToolContent: isOn(s.env[otelLogToolContent]),
	}

	// Connected means telemetry is on *and* pointed somewhere. A harness exporting to
	// another vendor's collector is deliberately not reported as connected to Terma —
	// the caller compares Endpoint against its own to decide.
	status.Connected = isOn(s.env[claudeEnableTelemetry]) && status.Endpoint != ""

	for _, sig := range AllSignals {
		var key string
		switch sig {
		case SignalTraces:
			key = otelTracesExporter
		case SignalLogs:
			key = otelLogsExporter
		case SignalMetrics:
			key = otelMetricsExporter
		}
		if _, exists := s.env[key]; c.root != "" && exists {
			status.HasPolicy = true
		}
	}

	// Traces need the beta flag as well as the exporter; without it the exporter is set
	// and no span is ever produced. Reporting traces as on in that state would send
	// someone hunting in Terma for data the harness never sent.
	status.Signals = claudeSignals(s.env)

	status.KeyPrefix = maskKeyFromHeaders(s.env[otelHeaders])
	// Helper mode keeps the key out of the settings file entirely; the prefix worth
	// reporting then lives in the helper script.
	if status.KeyPrefix == "" {
		if helper := stringSetting(s.root, claudeOtelHeadersHelper); helper != "" && isOwnHelper(helper) {
			status.KeyPrefix = MaskKey(keyFromHelper(helper))
		}
	}
	// With a journal, ownership is value-based: a key edited since connect is the user's
	// and must not make a later disconnect fall back to deleting it. Without a journal,
	// retain the legacy name-based count so older installations can still be cleaned up.
	j, err := loadJournal(c.Name(), path)
	if err != nil {
		return Status{}, err
	}
	status.ProjectID = projectIDOf(s.env, j)
	if j != nil {
		for key, installed := range j.Installed {
			if current, ok := s.env[key]; ok && current == installed {
				status.ManagedKeys++
			}
		}
		for key, installed := range j.InstalledSettings {
			if stringSetting(s.root, key) == installed {
				status.ManagedKeys++
			}
		}
	} else {
		for _, key := range c.legacyKeys() {
			if _, ok := s.env[key]; ok {
				status.ManagedKeys++
			}
		}
	}

	// Compared against the endpoint actually configured here, so status reports whether
	// this file is internally consistent — not whether it agrees with some other project.
	status.Conflicts = claudeConflicts(s.env, s.root, Exporter{
		Endpoint: status.Endpoint,
		Signals:  status.Signals,
	}, c.layer())
	return status, nil
}

// ConflictsWith reports what in the existing config would defeat the export e describes:
// a generic endpoint already pointing elsewhere, a per-signal override, or the
// detailed-beta-tracing redirect.
func (c Claude) ConflictsWith(e Exporter) ([]Conflict, error) {
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

// claudeConflicts finds every setting that would send data — and with it Terma's
// Authorization header — somewhere other than e.Endpoint, or break the export outright.
//
// e describes the export that will be in effect: the one about to be written during a
// connect, or the one already on disk when reporting status. l is the file being
// examined and what outranks it; settings found in the file itself are clearable,
// settings in an outranking layer are not.
func claudeConflicts(env map[string]string, root map[string]json.RawMessage, e Exporter, l claudeLayer) []Conflict {
	var out []Conflict

	// Not an env entry, so nothing above would find it. Terma's own helper is exempt:
	// it is the credential delivery this connect manages, not a foreign override.
	if helper := stringSetting(root, claudeOtelHeadersHelper); helper != "" && !isOwnHelper(helper) {
		out = append(out, Conflict{
			Key:        claudeOtelHeadersHelper,
			Value:      helper,
			Reason:     "supplies its own OTLP headers, which decide what the export authenticates with instead of Terma's key",
			Credential: true,
			Scope:      l.scope,
			Clearable:  true,
		})
	}

	// The generic endpoint pointing at another collector is not a disclosure — it is
	// overwritten — but a connect would replace an export the user set up. Reported
	// whether or not telemetry is currently switched on: a disabled config still holds
	// the destination someone chose, and connect would overwrite it just the same.
	if current := env[otelEndpoint]; current != "" && current != e.Endpoint {
		reason := "telemetry is already exporting here; connecting replaces it"
		if !isOn(env[claudeEnableTelemetry]) {
			reason = "a previously configured destination; connecting replaces it"
		}
		out = append(out, Conflict{
			Key: otelEndpoint, Value: current, Reason: reason,
			Scope: l.scope, Clearable: true,
		})
	}

	for _, o := range perSignalOverrides {
		// A signal Terma is not exporting cannot be redirected away from Terma.
		if !e.HasSignal(o.signal) {
			continue
		}
		// Compared against the per-signal URL, not the base. The generic endpoint gets
		// `/v1/<signal>` appended; a per-signal variable does not, so the bare base URL
		// here posts to the wrong path and the suffixed URL is the only correct value.
		if v := env[o.endpoint]; v != "" && v != e.SignalEndpoint(o.signal) {
			reason := "overrides the endpoint for " + string(o.signal) + ", which would receive Terma's credential"
			if v == e.Endpoint {
				reason = "is Terma's base URL, which a per-signal endpoint does not append /v1/" +
					string(o.signal) + " to — " + string(o.signal) + " would post to the wrong path"
			}
			out = append(out, Conflict{
				Key:        o.endpoint,
				Value:      v,
				Reason:     reason,
				Credential: v != e.Endpoint,
				Scope:      l.scope,
				Clearable:  true,
			})
		}
		if v := env[o.headers]; v != "" {
			// The value is a header bag that may itself hold a credential, so it is
			// named but never printed.
			out = append(out, Conflict{
				Key:       o.headers,
				Reason:    "merges into the headers for " + string(o.signal) + ", overriding Terma's",
				Scope:     l.scope,
				Clearable: true,
			})
		}
		if v := env[o.protocol]; v != "" && v != protocolHTTPProtobuf {
			out = append(out, Conflict{
				Key:       o.protocol,
				Value:     v,
				Reason:    "sends " + string(o.signal) + " over a protocol Terma's endpoint does not serve",
				Scope:     l.scope,
				Clearable: true,
			})
		}
	}

	// Detailed beta tracing diverts logs and traces to its own endpoint instead of the
	// exporters, so it defeats the connect without touching a single OTEL_* variable.
	// Only logs and traces move; a metrics-only connect is unaffected.
	//
	// Both halves are required for it to do anything: a saved endpoint with the switch
	// off is dormant, and blocking on that would be a false positive that also talks
	// --force into deleting two settings for no reason.
	if endpoint := env[claudeBetaTracingEndpoint]; endpoint != "" &&
		isOn(env[claudeBetaTracingDetailed]) &&
		(e.HasSignal(SignalTraces) || e.HasSignal(SignalLogs)) {
		out = append(out, Conflict{
			Key:    claudeBetaTracingEndpoint,
			Value:  endpoint,
			Reason: "detailed beta tracing sends logs and traces here instead of to Terma",
			// Whether this carries OTEL_EXPORTER_OTLP_HEADERS is undocumented. Assumed
			// yes: the safe assumption for a redirect is that the credential follows it,
			// and Anthropic's managed settings strip this variable alongside credentials.
			Credential: true,
			Scope:      l.scope,
			Clearable:  true,
		})
	}

	// Everything above is in the file Terma owns. These are not, and outrank it.
	out = append(out, environmentConflicts(e)...)
	out = append(out, projectConflicts(e, l)...)
	// Advisory, and last: an outranking scope that silently turns content capture off.
	out = append(out, captureConflictsIn(e, l)...)
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

// Connect merges Terma's variables into the settings file, leaving every other
// setting — hooks, permissions, model, statusLine — untouched.
//
// clearConflicts removes the per-signal overrides reported by ConflictsWith. Without it
// they are left alone, which is why the caller must refuse to connect while any remain:
// writing the credential and leaving the override in place is the leak.
func (c Claude) Connect(e Exporter, clearConflicts bool) error {
	env := c.render(e)
	// The top-level settings this connect owns, alongside the env block. Today that is
	// only the headers helper; recorded in the journal the same way env keys are.
	settings := map[string]string{}
	if e.HelperPath != "" {
		settings[claudeOtelHeadersHelper] = e.HelperPath
	}

	path, err := c.ConfigPath()
	if err != nil {
		return err
	}
	s, err := loadSettings(path)
	if err != nil {
		return err
	}
	previousJournal, err := loadJournal(c.Name(), path)
	if err != nil {
		return err
	}

	cleared := map[string]string{}
	clearedSettings := map[string]string{}
	if clearConflicts {
		for _, conflict := range claudeConflicts(s.env, s.root, e, c.layer()) {
			// Only this file's settings are Terma's to touch. A shell export or a
			// project file is somebody else's, and the caller refuses the connect rather
			// than pretending --force dealt with it.
			if !conflict.Clearable {
				continue
			}
			// The generic endpoint is about to be overwritten by the merge anyway;
			// deleting it here would be a no-op with a confusing name.
			if conflict.Key == otelEndpoint {
				continue
			}
			if conflict.Key == claudeOtelHeadersHelper {
				delete(s.root, claudeOtelHeadersHelper)
				clearedSettings[claudeOtelHeadersHelper] = conflict.Value
				continue
			}
			if value, ok := s.env[conflict.Key]; ok {
				cleared[conflict.Key] = value
			}
			delete(s.env, conflict.Key)
			// The beta-tracing switch and its endpoint are a pair — the switch alone
			// does nothing, so leaving it behind would just be dead config.
			if conflict.Key == claudeBetaTracingEndpoint {
				if value, ok := s.env[claudeBetaTracingDetailed]; ok {
					cleared[claudeBetaTracingDetailed] = value
				}
				delete(s.env, claudeBetaTracingDetailed)
			}
		}
	}

	// Managed keys this render deliberately omits: the credential in helper mode, the
	// beta switch when traces are off, resource attributes when there are none. A
	// value left over from an earlier connect is not inert — OTEL_EXPORTER_OTLP_HEADERS
	// is read by the SDK directly and outranks the helper script, so a key minted for a
	// previous project keeps winning and every export lands in the wrong place while
	// everything reports as connected. Clearing them makes a reconnect converge on
	// exactly what Render produced.
	//
	// Only this scope's keys: at repository scope the render omits the endpoint and
	// headers by design, and a project file that holds its own is not Terma's to empty.
	//
	// A retired key (claudeRetiredKeys) goes the same way, but only while it still
	// holds what an earlier connect installed. A managed key is cleared whoever set it,
	// because a stale value there defeats the connect; a retired key the user set is
	// simply theirs.
	for _, key := range c.legacyKeys() {
		if _, rendered := env[key]; rendered {
			continue
		}
		value, present := s.env[key]
		if !present {
			continue
		}
		// What disconnect should put back: for a value an earlier connect installed,
		// the pre-Terma value it recorded — never Terma's own leftover.
		restore := value
		owned := false
		if previousJournal != nil {
			if installed, ok := previousJournal.Installed[key]; ok && installed == value {
				owned = true
				restore = ""
				if prior := previousJournal.Previous[key]; prior != nil {
					restore = *prior
				}
			}
		}
		if !owned && slices.Contains(c.retiredKeys(), key) {
			continue
		}
		if restore != "" {
			cleared[key] = restore
		}
		delete(s.env, key)
	}

	// Recorded before the merge overwrites anything, so disconnect can put the previous
	// values back rather than inferring which keys were Terma's.
	j := newJournal(c.Name(), path, s.env, env, cleared, clearedSettings, previousJournal)
	j.ProjectID = e.ProjectID
	for key, value := range settings {
		j.InstalledSettings[key] = value
		current := stringSetting(s.root, key)
		// Ownership carries across a reconnect the way it does for env keys: while the
		// file still holds what the earlier connect installed, the value to put back is
		// the pre-Terma one that connect recorded — not Terma's own helper path,
		// which a later disconnect would otherwise "restore", leaving the setting
		// pointing at a script it had just deleted.
		if previousJournal != nil {
			if priorInstalled, owned := previousJournal.InstalledSettings[key]; owned && current == priorInstalled {
				j.PreviousSettings[key] = cloneString(previousJournal.PreviousSettings[key])
				continue
			}
		}
		if current != "" {
			current := current
			j.PreviousSettings[key] = &current
		} else {
			j.PreviousSettings[key] = nil
		}
	}

	// The helper script goes down first: the settings file about to be written points
	// at it, and a harness starting between the two writes must find the credential
	// already there. Idempotent, so a re-run after a crash converges.
	if e.HelperPath != "" {
		if err := writeHelper(e.HelperPath, e.APIKey); err != nil {
			return err
		}
	}

	// Persist ownership first. If this fails, the credential-bearing settings have not
	// changed. A crash after this point but before the settings write leaves a harmless
	// journal whose installed values do not match, rather than an unjournaled credential.
	if err := j.save(); err != nil {
		return err
	}
	s.merge(env)
	for key, value := range settings {
		encoded, err := marshalJSON(value, "")
		if err != nil {
			return err
		}
		s.root[key] = encoded
	}
	// tighten only when the merged env carries the server key inline. In helper mode
	// the settings file holds a path, not a secret, and a repository's local layer
	// holds no key at all, so the user's own mode survives — a dotfiles-friendly 0644,
	// or the 0644 a committed file wants.
	_, inlineKey := env[otelHeaders]
	defer pruneJournals()
	if err := s.save(inlineKey); err != nil {
		// Put the preceding journal back so a failed reconnect does not replace the
		// ownership record for the still-current installation.
		var rollbackErr error
		if previousJournal != nil {
			rollbackErr = previousJournal.save()
		} else {
			rollbackErr = deleteJournal(c.Name(), path)
		}
		if rollbackErr != nil {
			return fmt.Errorf("write settings: %w (also failed to restore telemetry journal: %w)", err, rollbackErr)
		}
		return err
	}
	return nil
}

// Disconnect undoes what the recorded connect did.
//
// With a journal it restores each key to the value it held beforehand and leaves alone
// any key edited since — a config someone has adjusted is their decision, not stale
// Terma state. Without one (an older connect, a hand-edited config) it falls back to
// removing the managed keys, which is the best that can be done without a record and is
// reported as such.
func (c Claude) Disconnect() (DisconnectResult, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return DisconnectResult{}, err
	}
	s, err := loadSettings(path)
	if err != nil {
		return DisconnectResult{}, err
	}

	j, err := loadJournal(c.Name(), path)
	if err != nil {
		return DisconnectResult{}, err
	}

	var result DisconnectResult
	var remaining *journal
	switch {
	// loadJournal only returns a record written for this exact config, so a non-nil
	// journal is by construction the right one.
	case j != nil:
		result, remaining = j.apply(s.env)
		// Top-level settings Terma installed: same ownership rule as env keys. The
		// helper file itself goes with its setting — it holds the credential, and a
		// disconnect that left it behind would strand a live key on disk.
		for key, installed := range j.InstalledSettings {
			current := stringSetting(s.root, key)
			if current != installed {
				if current != "" {
					result.Skipped = append(result.Skipped, key)
					remaining.InstalledSettings[key] = installed
					remaining.PreviousSettings[key] = cloneString(j.PreviousSettings[key])
				}
				continue
			}
			if key == claudeOtelHeadersHelper && isOwnHelper(installed) {
				if err := deleteHelper(installed); err != nil {
					return result, err
				}
			}
			if prior := j.PreviousSettings[key]; prior != nil {
				encoded, err := marshalJSON(*prior, "")
				if err != nil {
					return result, err
				}
				s.root[key] = encoded
				result.Restored++
			} else {
				delete(s.root, key)
				result.Removed++
			}
		}
		for key, value := range j.ClearedSettings {
			if _, taken := s.root[key]; taken {
				result.Skipped = append(result.Skipped, key)
				remaining.ClearedSettings[key] = value
				continue
			}
			encoded, err := marshalJSON(value, "")
			if err != nil {
				return result, err
			}
			s.root[key] = encoded
			result.Restored++
		}
	default:
		// No record: an older connect, a hand-edited config — or, at repository scope,
		// a project file committed by a colleague whose journal is on their machine.
		result.Removed = s.remove(c.legacyKeys())
		result.Unjournaled = true
	}

	sort.Strings(result.Skipped)
	defer pruneJournals()
	if result.Removed > 0 || result.Restored > 0 {
		// The credential is gone, so there is nothing left to tighten for.
		if err := s.save(false); err != nil {
			return result, err
		}
	}

	if j == nil {
		return result, nil
	}
	if remaining != nil && !remaining.empty() {
		return result, remaining.save()
	}
	return result, deleteJournal(c.Name(), path)
}

// Backup exposes the pre-modification copy so `connect` can tell the user where it is.
//
// Whether the stored backup may be replaced comes from the journal, not from what the
// endpoint happens to say. Reading ownership off the endpoint gets it wrong both ways: a
// config someone wrote by hand against the Terma endpoint looks like Terma's, so its
// backup is kept stale and the config itself is later deleted; a Terma config whose
// endpoint was edited out looks like the user's, so it overwrites the real original.
//
// endpoint is retained for the no-journal case — an older connect, or a hand-edited
// config — where the endpoint is the only evidence available.
//
// The rule the journal expresses: snapshot whatever Terma did not write, and leave the
// snapshot alone when re-connecting over a config this CLI installed — including after a
// disconnect-and-reconfigure cycle, where a never-overwritten backup would otherwise go
// stale and the next disconnect would delete the only live copy.
func (c Claude) Backup(endpoint string) (string, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return "", err
	}
	s, err := loadSettings(path)
	if err != nil {
		return "", err
	}

	// A journal for this exact file means the current contents are Terma's own work,
	// so the stored backup is still the pre-Terma state and must be kept.
	j, err := loadJournal(c.Name(), path)
	if err != nil {
		return "", err
	}
	if j != nil {
		return s.backup(false)
	}
	// No record: fall back to the endpoint, which is the only evidence there is.
	return s.backup(s.env[otelEndpoint] != endpoint)
}

// ManagedKeys is what Disconnect would remove, for a preview.
func (c Claude) ManagedKeys() []string { return c.managedKeys() }

// isOn treats Claude Code's accepted truthy spellings as on, and everything else —
// including "0", "false", and absent — as off.
func isOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func withoutSignal(signals []Signal, drop Signal) []Signal {
	out := signals[:0]
	for _, s := range signals {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// keyFromHeaders extracts the raw key from an OTEL_EXPORTER_OTLP_HEADERS value, or ""
// when none is configured. Callers that display it must mask it; the raw form exists
// for reuse, where handing back a masked key would mint an orphan instead.
func keyFromHeaders(headers string) string {
	for pair := range strings.SplitSeq(headers, ",") {
		name, value, found := strings.Cut(pair, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "Authorization") {
			continue
		}
		key := strings.TrimSpace(value)
		return strings.TrimSpace(strings.TrimPrefix(key, "Bearer"))
	}
	return ""
}

// CurrentCredential returns the key this config already presents to endpoint for
// projectID, whichever way it is delivered — helper script or inline header. It is the
// reuse path: a reconnect that minted a fresh key every time would leave a trail of
// live orphans nobody remembers, and the key already here is exactly as scoped as the
// one a mint would produce. Both endpoint and project must match: a key for another
// project would be rejected server-side, and one for another deployment would be
// disclosed to it.
func (c Claude) CurrentCredential(endpoint, projectID string) (string, bool) {
	path, err := c.ConfigPath()
	if err != nil {
		return "", false
	}
	s, err := loadSettings(path)
	if err != nil {
		return "", false
	}
	if s.env[otelEndpoint] != endpoint {
		return "", false
	}
	j, err := loadJournal(c.Name(), path)
	if err != nil || projectIDOf(s.env, j) != projectID {
		return "", false
	}
	if helper := stringSetting(s.root, claudeOtelHeadersHelper); helper != "" && isOwnHelper(helper) {
		if key := keyFromHelper(helper); key != "" {
			return key, true
		}
	}
	if key := keyFromHeaders(s.env[otelHeaders]); serverkey.Is(key) {
		return key, true
	}
	return "", false
}

// maskKeyFromHeaders extracts the key from an OTEL_EXPORTER_OTLP_HEADERS value and
// returns only its head. The full key is never returned: status output lands in
// terminals, screenshots, and bug reports.
func maskKeyFromHeaders(headers string) string {
	return MaskKey(keyFromHeaders(headers))
}

// MaskKey renders a credential as a recognizable but unusable prefix.
func MaskKey(key string) string {
	if key == "" {
		return ""
	}
	return serverkey.Mask(key)
}

// projectIDOf is the project a configuration reports to. The connect journal is the
// record. A configuration connected before the journal carried it still holds the id
// in the OTEL_RESOURCE_ATTRIBUTES an older Terma wrote, and that answers until the next
// connect replaces both; a hand-assembled configuration has neither and reads as
// unknown.
func projectIDOf(env map[string]string, j *journal) string {
	if j != nil && j.ProjectID != "" {
		return j.ProjectID
	}
	return resourceAttribute(env[otelResourceAttributes], AttrProjectID)
}

// resourceAttribute reads one key out of an OTEL_RESOURCE_ATTRIBUTES value, which
// only the legacy fallback in projectIDOf still has reason to do.
func resourceAttribute(raw, key string) string {
	for pair := range strings.SplitSeq(raw, ",") {
		name, value, found := strings.Cut(pair, "=")
		if found && strings.TrimSpace(name) == key {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
