package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// WriteRouteSettings writes a route's headers-helper script and a settings document naming it, both
// in terma's directory and passed as `claude --settings <path>`: that outranks a machine-wide `env`
// block and otelHeadersHelper, which beat the process environment, and keeps the key out of every
// tool subprocess's environment.
func (exporter) WriteRouteSettings(settingsPath, helperPath string, e harness.Exporter) error {
	if err := harness.WriteHelper(helperPath, e.APIKey); err != nil {
		return err
	}
	e.HelperPath = helperPath
	env := exporter{}.render(e)
	// A route owns its destination, signal overrides included; empty headers let the helper supply the key.
	env[harness.EnvOTLPHeaders] = ""
	for _, override := range perSignalOverrides {
		env[override.endpoint] = e.SignalEndpoint(override.signal)
		env[override.protocol] = harness.ProtocolHTTPProtobuf
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
