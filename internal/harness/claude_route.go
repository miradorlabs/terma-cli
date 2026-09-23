package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// WriteRouteSettings writes the two files a per-repository route hands Claude Code: a
// headers-helper script holding the key, and a settings document that names it beside
// the export's `env` block. Neither is Claude Code's own settings file — they live in
// Terma's directory and reach the agent on its command line (`claude --settings <path>`).
//
// The command line is the point. A developer who connected Claude Code machine-wide has
// an `env` block and an otelHeadersHelper in ~/.claude/settings.json, and both outrank
// the process environment: exporting OTEL_EXPORTER_OTLP_HEADERS around the agent does
// nothing while a user-level helper exists, so every session keeps reporting to the
// machine-wide project and nothing says so. `--settings` ranks above the user file on
// both counts. Verified against local mock API/OTLP receivers on Claude Code 2.1.270–2.1.272:
// explicit settings also override global detailed beta tracing, whereas native
// repository-local settings do not reliably override that exporter.
//
// It is also the better home for the key: a 0700 script, rather than a variable every
// tool subprocess the agent starts would inherit.
func (Claude) WriteRouteSettings(settingsPath, helperPath string, e Exporter) error {
	if err := writeHelper(helperPath, e.APIKey); err != nil {
		return err
	}
	e.HelperPath = helperPath
	env := Claude{}.render(e)
	// A route owns its destination, including higher-priority signal overrides.
	// Empty headers allow the dedicated helper to supply the credential.
	env[otelHeaders] = ""
	for _, override := range perSignalOverrides {
		env[override.endpoint] = e.SignalEndpoint(override.signal)
		env[override.protocol] = protocolHTTPProtobuf
		env[override.headers] = ""
	}
	env[claudeBetaTracingDetailed] = "0"
	env[claudeBetaTracingEndpoint] = ""
	if env[claudeEnhancedTelemetry] == "" {
		env[claudeEnhancedTelemetry] = "0"
	}
	doc := struct {
		Env    map[string]string `json:"env"`
		Helper string            `json:"otelHeadersHelper"`
	}{Env: env, Helper: helperPath}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(settingsPath), err)
	}
	return config.WriteFileAtomic(settingsPath, append(data, '\n'), 0o600)
}
