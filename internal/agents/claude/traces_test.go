package claude

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Spans need the master switch, the beta switch and the traces exporter in the user settings
// file, and no running process's environment switching any of them off.
func TestTracesOffNamesWhatKeepsSpans(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	user := filepath.Join(dir, "settings.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(user, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, env string
		want      []string
	}{
		{"all on", `"CLAUDE_CODE_ENABLE_TELEMETRY":"1","CLAUDE_CODE_ENHANCED_TELEMETRY_BETA":"1","OTEL_TRACES_EXPORTER":"otlp"`, nil},
		{"an earlier terma's: no beta switch", `"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_TRACES_EXPORTER":"otlp"`,
			[]string{"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1"}},
		{"telemetry off", `"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA":"1","OTEL_TRACES_EXPORTER":"none"`,
			[]string{"CLAUDE_CODE_ENABLE_TELEMETRY=1", "OTEL_TRACES_EXPORTER=otlp"}},
	} {
		write(`{"env":{` + tc.env + `}}`)
		if off, err := (Agent{}).TracesOff(); err != nil || !slices.Equal(off, tc.want) {
			t.Errorf("%s: TracesOff = %q, %v; want %q", tc.name, off, err, tc.want)
		}
	}
	write(`{"env":`)
	if _, err := (Agent{}).TracesOff(); err == nil {
		t.Error("an unreadable settings file read as exporting spans")
	}

	env := []string{"HOME=/home/dev", "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=0", "OTEL_TRACES_EXPORTER=none", "OTEL_LOGS_EXPORTER=otlp", "CLAUDE_CODE_ENABLE_TELEMETRY="}
	if off := (Agent{}).TracesOffIn(env); !slices.Equal(off, []string{claudeEnhancedTelemetry, otelTracesExporter}) {
		t.Errorf("process env: TracesOffIn = %q", off)
	}
	if off := (Agent{}).TracesOffIn([]string{"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1", "OTEL_TRACES_EXPORTER=otlp", "HOME=/"}); len(off) != 0 {
		t.Errorf("process env on: TracesOffIn = %q", off)
	}
}
