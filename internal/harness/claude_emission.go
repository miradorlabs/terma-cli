package harness

import (
	"path/filepath"
)

// EmissionStatus reads the user and repository settings together, including the
// developer's settings.local.json. Unlike Status, it describes the combined export
// switches rather than one file. It does not prove a running agent has reloaded them,
// and cannot inspect managed settings or an arbitrary --settings argument.
func (c Claude) EmissionStatus(root string) (Status, error) {
	path, err := (Claude{}).ConfigPath()
	if err != nil {
		return Status{}, err
	}
	paths := []string{path}
	if root != "" {
		paths = append(paths, filepath.Join(root, claudeProjectSettings), filepath.Join(root, claudeProjectLocalSettings))
	}
	env := map[string]string{}
	st := Status{ConfigPath: path}
	for _, path := range paths {
		s, err := loadSettings(path)
		if err != nil {
			return Status{}, err
		}
		for key, value := range s.env {
			env[key] = value
			switch key {
			case claudeEnableTelemetry, claudeEnhancedTelemetry, otelTracesExporter, otelLogsExporter, otelMetricsExporter:
				st.ConfigPath = path
			}
		}
	}
	st.Endpoint = env[otelEndpoint]
	st.Connected = isOn(env[claudeEnableTelemetry]) && st.Endpoint != ""
	st.Signals = claudeSignals(env)
	return st, nil
}

func claudeSignals(env map[string]string) []Signal {
	var signals []Signal
	if env[otelTracesExporter] == exporterOTLP && isOn(env[claudeEnhancedTelemetry]) {
		signals = append(signals, SignalTraces)
	}
	if env[otelLogsExporter] == exporterOTLP {
		signals = append(signals, SignalLogs)
	}
	if env[otelMetricsExporter] == exporterOTLP {
		signals = append(signals, SignalMetrics)
	}
	return signals
}
