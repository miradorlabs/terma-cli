package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// exporter configures Claude Code's OpenTelemetry export through the `env` block of its
// settings file; the zero value acts on the user file, Local binds it to a repository.
type exporter struct {
	root string
}

// claudeProjectSettings is the committed project settings file, which Claude Code applies
// over the user file.
const claudeProjectSettings = ".claude/settings.json"

// claudeProjectLocalSettings is the gitignored per-developer project file; terma only scans it.
const claudeProjectLocalSettings = ".claude/settings.local.json"

// Local returns the harness bound to the repository at root.
func (exporter) Local(root string) harness.Harness { return exporter{root: root} }

// Scope reports which layer this value acts on.
func (c exporter) Scope() harness.Scope {
	if c.root != "" {
		return harness.ScopeLocal
	}
	return harness.ScopeGlobal
}

const (
	// claudeEnableTelemetry is the master switch; without it every OTEL_* variable is inert.
	claudeEnableTelemetry = "CLAUDE_CODE_ENABLE_TELEMETRY"
	// claudeEnhancedTelemetry gates spans: OTEL_TRACES_EXPORTER alone produces none.
	claudeEnhancedTelemetry = "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA"

	otelTracesExporter  = "OTEL_TRACES_EXPORTER"
	otelLogsExporter    = "OTEL_LOGS_EXPORTER"
	otelMetricsExporter = "OTEL_METRICS_EXPORTER"

	// The content switches are written either way, so the file states the redaction posture.
	otelLogUserPrompts       = "OTEL_LOG_USER_PROMPTS"
	otelLogAssistantResponse = "OTEL_LOG_ASSISTANT_RESPONSES"
	otelLogToolDetails       = "OTEL_LOG_TOOL_DETAILS"
	otelLogToolContent       = "OTEL_LOG_TOOL_CONTENT"

	// A disabled signal is written as "none" rather than omitted.
	exporterOTLP = "otlp"
	exporterNone = "none"

	// Together these send logs and traces to BETA_TRACING_ENDPOINT instead of the configured
	// exporters, bypassing OTEL_EXPORTER_OTLP_ENDPOINT, so a check of the OTLP variables misses it.
	claudeBetaTracingDetailed = "ENABLE_BETA_TRACING_DETAILED"
	claudeBetaTracingEndpoint = "BETA_TRACING_ENDPOINT"

	// claudeOtelHeadersHelper is a top-level setting, not an env entry, and supplies the same
	// Authorization header terma writes.
	claudeOtelHeadersHelper = "otelHeadersHelper"
)

// perSignalOverrides outrank the generic variables and receive the merged generic headers,
// so a stale one pointing at another vendor gets terma's key; terma reports them, never writes
// them, and removes them only when asked.
var perSignalOverrides = []struct {
	endpoint, protocol, headers string
	signal                      harness.Signal
}{
	{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", harness.SignalTraces},
	{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", harness.SignalLogs},
	{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", harness.SignalMetrics},
}

// claudeManagedKeys is every variable terma sets and exactly what disconnect removes; the
// per-signal overrides are not terma's and are absent on purpose.
var claudeManagedKeys = []string{
	claudeEnableTelemetry,
	claudeEnhancedTelemetry,
	otelTracesExporter,
	otelLogsExporter,
	otelMetricsExporter,
	harness.EnvOTLPProtocol,
	harness.EnvOTLPEndpoint,
	harness.EnvOTLPHeaders,
	otelLogUserPrompts,
	otelLogAssistantResponse,
	otelLogToolDetails,
	otelLogToolContent,
}

// claudeLocalKeys is what a repository-scope connect writes, clears and removes: what to ship,
// never where or with which credential. The beta switch is here because traces need it.
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

func (c exporter) managedKeys() []string {
	if c.root != "" {
		return claudeLocalKeys
	}
	return claudeManagedKeys
}

// Name is the token `terma connect` and `--harness` accept.
func (exporter) Name() string { return "claude" }

// ServiceName is Claude Code's own default; terma writes no OTEL_RESOURCE_ATTRIBUTES.
func (exporter) ServiceName() string { return "claude-code" }

// DisplayName is how the agent is written in prose.
func (exporter) DisplayName() string { return "Claude Code" }

// SupportsHeadersHelper is true: Claude Code has the otelHeadersHelper setting.
func (exporter) SupportsHeadersHelper() bool { return true }

// Detect runs `claude --version`; a missing binary is not-found, not an error.
func (exporter) Detect(ctx context.Context) harness.Detection {
	return harness.DetectBinary(ctx, "claude", harness.SemverRE)
}

// ConfigPath is the user settings file (honouring CLAUDE_CONFIG_DIR, or a connect silently does
// nothing), or the repository's .claude/settings.json when bound to one.
func (c exporter) ConfigPath() (string, error) {
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

// render maps an Exporter onto Claude Code's variables, cut down to claudeLocalKeys at
// repository scope.
func (c exporter) render(e harness.Exporter) map[string]string {
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

func renderClaude(e harness.Exporter) map[string]string {
	traces := e.HasSignal(harness.SignalTraces)

	env := map[string]string{
		claudeEnableTelemetry: "1",

		otelTracesExporter:  exporterFor(traces),
		otelLogsExporter:    exporterFor(e.HasSignal(harness.SignalLogs)),
		otelMetricsExporter: exporterFor(e.HasSignal(harness.SignalMetrics)),

		harness.EnvOTLPProtocol: harness.ProtocolHTTPProtobuf,
		harness.EnvOTLPEndpoint: e.Endpoint,

		otelLogUserPrompts:       boolValue(e.IncludePrompts),
		otelLogAssistantResponse: boolValue(e.IncludePrompts),
		otelLogToolDetails:       boolValue(e.IncludeToolContent),
		otelLogToolContent:       boolValue(e.IncludeToolContent),
	}

	// Only with traces on, so a logs-and-metrics connect opts no one into a beta.
	if traces {
		env[claudeEnhancedTelemetry] = "1"
	}

	// In helper mode the credential stays out of the settings file.
	if e.APIKey != "" && e.HelperPath == "" {
		env[harness.EnvOTLPHeaders] = "Authorization=Bearer " + e.APIKey
	}
	// e.ResourceAttributes are not rendered: OTEL_RESOURCE_ATTRIBUTES belongs to the user.
	return env
}

// renderedByTerma reports whether value is one renderClaude writes for a repository
// policy key: the only evidence a policy without a journal is Terma's to remove.
func renderedByTerma(key, value string) bool {
	switch key {
	case otelTracesExporter, otelLogsExporter, otelMetricsExporter:
		return value == exporterFor(true) || value == exporterFor(false)
	case otelLogUserPrompts, otelLogAssistantResponse, otelLogToolDetails, otelLogToolContent:
		return value == boolValue(true) || value == boolValue(false)
	case claudeEnhancedTelemetry:
		return value == "1"
	}
	return false
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
func (c exporter) Status() (harness.Status, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return harness.Status{}, err
	}
	s, err := loadSettings(path)
	if err != nil {
		return harness.Status{}, err
	}

	status := harness.Status{
		ConfigPath: path,
		Exists:     s.existed,
		Endpoint:   s.env[harness.EnvOTLPEndpoint],

		// Absent is off, as in Claude Code.
		IncludePrompts:     isOn(s.env[otelLogUserPrompts]),
		IncludeToolContent: isOn(s.env[otelLogToolContent]),
	}

	// The caller compares Endpoint against its own to decide whether this is terma.
	status.Connected = isOn(s.env[claudeEnableTelemetry]) && status.Endpoint != ""

	for _, sig := range harness.AllSignals {
		var key string
		switch sig {
		case harness.SignalTraces:
			key = otelTracesExporter
		case harness.SignalLogs:
			key = otelLogsExporter
		case harness.SignalMetrics:
			key = otelMetricsExporter
		}
		if _, exists := s.env[key]; c.root != "" && exists {
			status.HasPolicy = true
		}
	}

	status.Signals = claudeSignals(s.env)

	status.KeyPrefix = maskKeyFromHeaders(s.env[harness.EnvOTLPHeaders])
	// In helper mode the key lives in the helper script.
	if status.KeyPrefix == "" {
		if helper := stringSetting(s.root, claudeOtelHeadersHelper); helper != "" && harness.IsOwnHelper(helper) {
			status.KeyPrefix = harness.MaskKey(harness.KeyFromHelper(helper))
		}
	}
	// With a journal, ownership is by value: a key edited since connect is the user's. A committed
	// repository policy may have no local journal.
	j, err := harness.LoadJournal(c.Name(), path)
	if err != nil {
		return harness.Status{}, err
	}
	status.ProjectID = projectIDOf(j)
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
	} else if c.root != "" {
		for _, key := range c.managedKeys() {
			if _, ok := s.env[key]; ok {
				status.ManagedKeys++
			}
		}
	}

	status.Conflicts = claudeConflicts(s.env, s.root, harness.Exporter{
		Endpoint: status.Endpoint,
		Signals:  status.Signals,
	}, c.layer())
	return status, nil
}

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

// Connect merges terma's variables into the settings file, leaving every other setting alone.
// clearConflicts removes the clearable conflicts; without it the caller must refuse while any remain.
func (c exporter) Connect(e harness.Exporter, clearConflicts bool) error {
	env := c.render(e)
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
	previousJournal, err := harness.LoadJournal(c.Name(), path)
	if err != nil {
		return err
	}

	cleared := map[string]string{}
	clearedSettings := map[string]string{}
	if clearConflicts {
		for _, conflict := range claudeConflicts(s.env, s.root, e, c.layer()) {
			// Only this file is terma's to touch; the caller refuses a connect over the rest.
			if !conflict.Clearable {
				continue
			}
			// The merge overwrites the generic endpoint anyway.
			if conflict.Key == harness.EnvOTLPEndpoint {
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
			// The switch alone does nothing, so it goes with its endpoint.
			if conflict.Key == claudeBetaTracingEndpoint {
				if value, ok := s.env[claudeBetaTracingDetailed]; ok {
					cleared[claudeBetaTracingDetailed] = value
				}
				delete(s.env, claudeBetaTracingDetailed)
			}
		}
	}

	// Clear managed keys this render omits: a leftover OTEL_EXPORTER_OTLP_HEADERS outranks the helper,
	// so a previous project's key keeps winning while everything reports connected. Only this scope's keys.
	for _, key := range c.managedKeys() {
		if _, rendered := env[key]; rendered {
			continue
		}
		value, present := s.env[key]
		if !present {
			continue
		}
		// Restore the pre-terma value an earlier connect recorded, never terma's own leftover.
		restore := value
		if previousJournal != nil {
			if installed, ok := previousJournal.Installed[key]; ok && installed == value {
				restore = ""
				if prior := previousJournal.Previous[key]; prior != nil {
					restore = *prior
				}
			}
		}
		if restore != "" {
			cleared[key] = restore
		}
		delete(s.env, key)
	}

	// Recorded before the merge, so disconnect can put the previous values back.
	j := harness.NewJournal(c.Name(), path, s.env, env, cleared, clearedSettings, previousJournal)
	j.ProjectID = e.ProjectID
	for key, value := range settings {
		j.InstalledSettings[key] = value
		current := stringSetting(s.root, key)
		// While the file still holds what the earlier connect installed, keep that connect's pre-terma
		// value, or disconnect would "restore" terma's own deleted helper path.
		if previousJournal != nil {
			if priorInstalled, owned := previousJournal.InstalledSettings[key]; owned && current == priorInstalled {
				j.PreviousSettings[key] = harness.CloneString(previousJournal.PreviousSettings[key])
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

	// The helper first: the settings file about to be written points at it.
	if e.HelperPath != "" {
		if err := harness.WriteHelper(e.HelperPath, e.APIKey); err != nil {
			return err
		}
	}

	// The journal before the settings, so a crash never leaves an unjournaled credential.
	if err := j.Save(); err != nil {
		return err
	}
	s.merge(env)
	for key, value := range settings {
		encoded, err := hookmgr.MarshalJSON(value, "", "")
		if err != nil {
			return err
		}
		s.root[key] = encoded
	}
	// Tighten the mode only for an inline key; otherwise the user's own mode (a committed 0644) survives.
	_, inlineKey := env[harness.EnvOTLPHeaders]
	defer harness.PruneJournals()
	if err := s.save(inlineKey); err != nil {
		// Restore the previous journal so a failed reconnect keeps the current installation's record.
		var rollbackErr error
		if previousJournal != nil {
			rollbackErr = previousJournal.Save()
		} else {
			rollbackErr = harness.DeleteJournal(c.Name(), path)
		}
		if rollbackErr != nil {
			return fmt.Errorf("write settings: %w (also failed to restore telemetry journal: %w)", err, rollbackErr)
		}
		return err
	}
	return nil
}

// Disconnect undoes the recorded connect, leaving any key edited since alone. Without a journal,
// a repository policy loses only values terma writes (renderedByTerma); user settings are untouched.
func (c exporter) Disconnect() (harness.DisconnectResult, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return harness.DisconnectResult{}, err
	}
	s, err := loadSettings(path)
	if err != nil {
		return harness.DisconnectResult{}, err
	}

	j, err := harness.LoadJournal(c.Name(), path)
	if err != nil {
		return harness.DisconnectResult{}, err
	}

	var result harness.DisconnectResult
	var remaining *harness.Journal
	switch {
	case j != nil:
		result, remaining = j.Apply(s.env)
		// The helper file goes with its setting: it holds a live key.
		for key, installed := range j.InstalledSettings {
			current := stringSetting(s.root, key)
			if current != installed {
				if current != "" {
					result.Skipped = append(result.Skipped, key)
					remaining.InstalledSettings[key] = installed
					remaining.PreviousSettings[key] = harness.CloneString(j.PreviousSettings[key])
				}
				continue
			}
			if key == claudeOtelHeadersHelper && harness.IsOwnHelper(installed) {
				if err := harness.DeleteHelper(installed); err != nil {
					return result, err
				}
			}
			if prior := j.PreviousSettings[key]; prior != nil {
				encoded, err := hookmgr.MarshalJSON(*prior, "", "")
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
			encoded, err := hookmgr.MarshalJSON(value, "", "")
			if err != nil {
				return result, err
			}
			s.root[key] = encoded
			result.Restored++
		}
	case c.root != "":
		var rendered []string
		for _, key := range c.managedKeys() {
			if value, ok := s.env[key]; ok && renderedByTerma(key, value) {
				rendered = append(rendered, key)
			}
		}
		result.Removed = s.remove(rendered)
		result.Unjournaled = result.Removed > 0
	}

	sort.Strings(result.Skipped)
	defer harness.PruneJournals()
	if result.Removed > 0 || result.Restored > 0 {
		if err := s.save(false); err != nil {
			return result, err
		}
	}

	if j == nil {
		return result, nil
	}
	if remaining != nil && !remaining.Empty() {
		return result, remaining.Save()
	}
	return result, harness.DeleteJournal(c.Name(), path)
}

// Backup exposes the pre-modification copy. A journal decides whether it may be replaced, since
// the endpoint misjudges hand-written and edited configs; endpoint is the evidence without one.
func (c exporter) Backup(endpoint string) (string, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return "", err
	}
	s, err := loadSettings(path)
	if err != nil {
		return "", err
	}

	// A journal means the current contents are terma's, so the backup is still the pre-terma state.
	j, err := harness.LoadJournal(c.Name(), path)
	if err != nil {
		return "", err
	}
	if j != nil {
		return s.backup(false)
	}
	return s.backup(s.env[harness.EnvOTLPEndpoint] != endpoint)
}

// ManagedKeys is what Disconnect would remove, for a preview.
func (c exporter) ManagedKeys() []string { return c.managedKeys() }

// isOn treats Claude Code's truthy spellings as on and everything else, including absent, as off.
func isOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// keyFromHeaders returns the raw key from an OTEL_EXPORTER_OTLP_HEADERS value, for reuse;
// a display must mask it.
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

// CurrentCredential returns the key this config already presents to endpoint for projectID, so a
// reconnect reuses it instead of minting an orphan. Endpoint and project must both match.
func (c exporter) CurrentCredential(endpoint, projectID string) (string, bool) {
	path, err := c.ConfigPath()
	if err != nil {
		return "", false
	}
	s, err := loadSettings(path)
	if err != nil {
		return "", false
	}
	if s.env[harness.EnvOTLPEndpoint] != endpoint {
		return "", false
	}
	j, err := harness.LoadJournal(c.Name(), path)
	if err != nil || projectIDOf(j) != projectID {
		return "", false
	}
	if helper := stringSetting(s.root, claudeOtelHeadersHelper); helper != "" && harness.IsOwnHelper(helper) {
		if key := harness.KeyFromHelper(helper); serverkey.Is(key) {
			return key, true
		}
	}
	if key := keyFromHeaders(s.env[harness.EnvOTLPHeaders]); serverkey.Is(key) {
		return key, true
	}
	return "", false
}

// maskKeyFromHeaders returns only the key's head: status output lands in screenshots.
func maskKeyFromHeaders(headers string) string {
	return harness.MaskKey(keyFromHeaders(headers))
}

func projectIDOf(j *harness.Journal) string {
	if j != nil {
		return j.ProjectID
	}
	return ""
}

// A capability asked for by type assertion switches off in silence when its method drifts.
var (
	_ harness.Harness      = exporter{}
	_ harness.Credentialed = exporter{}
	_ harness.Backuper     = exporter{}
	_ harness.Scoped       = exporter{}
)
