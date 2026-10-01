package claude

import (
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// TelemetrySwitch is Claude Code's master switch, which only the user file holds.
func (exporter) TelemetrySwitch() string { return claudeEnableTelemetry }

// EmissionStatus combines the user, repository and settings.local.json switches; it cannot see
// managed settings or a --settings argument.
func (c exporter) EmissionStatus(root string) (harness.Status, error) {
	path, err := (exporter{}).ConfigPath()
	if err != nil {
		return harness.Status{}, err
	}
	paths := []string{path}
	if root != "" {
		paths = append(paths, filepath.Join(root, claudeProjectSettings), filepath.Join(root, claudeProjectLocalSettings))
	}
	env := map[string]string{}
	st := harness.Status{ConfigPath: path}
	for _, path := range paths {
		s, err := loadSettings(path)
		if err != nil {
			return harness.Status{}, err
		}
		for key, value := range s.env {
			env[key] = value
			switch key {
			case claudeEnableTelemetry, claudeEnhancedTelemetry, otelTracesExporter, otelLogsExporter, otelMetricsExporter:
				st.ConfigPath = path
			}
		}
	}
	st.Endpoint = env[harness.EnvOTLPEndpoint]
	st.Connected = isOn(env[claudeEnableTelemetry]) && st.Endpoint != ""
	st.Signals = claudeSignals(env)
	return st, nil
}

func claudeSignals(env map[string]string) []harness.Signal {
	var signals []harness.Signal
	if env[otelTracesExporter] == exporterOTLP && isOn(env[claudeEnhancedTelemetry]) {
		signals = append(signals, harness.SignalTraces)
	}
	if env[otelLogsExporter] == exporterOTLP {
		signals = append(signals, harness.SignalLogs)
	}
	if env[otelMetricsExporter] == exporterOTLP {
		signals = append(signals, harness.SignalMetrics)
	}
	return signals
}
