package claude

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Spans need the beta switch and the traces exporter in the settings that apply, and no
// running process's environment switching either off.
func TestTracesOffNamesWhatKeepsSpans(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	root := t.TempDir()
	write := func(path, env string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"env":{`+env+`}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	user := filepath.Join(dir, "settings.json")
	on := `"CLAUDE_CODE_ENABLE_TELEMETRY":"1","CLAUDE_CODE_ENHANCED_TELEMETRY_BETA":"1","OTEL_TRACES_EXPORTER":"otlp"`

	write(user, on)
	if off := (Agent{}).TracesOff(root, nil); len(off) != 0 {
		t.Fatalf("both on: TracesOff = %q", off)
	}
	// An earlier terma wrote no beta switch.
	write(user, `"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_TRACES_EXPORTER":"otlp"`)
	if off := (Agent{}).TracesOff(root, nil); len(off) != 1 {
		t.Fatalf("no beta switch: TracesOff = %q", off)
	}
	// A repository switching traces off.
	write(user, on)
	write(filepath.Join(root, claudeProjectSettings), `"OTEL_TRACES_EXPORTER":"none"`)
	if off := (Agent{}).TracesOff(root, nil); len(off) != 1 {
		t.Fatalf("repository off: TracesOff = %q", off)
	}

	env := []string{"HOME=/home/dev", "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=0", "OTEL_TRACES_EXPORTER=none", "OTEL_LOGS_EXPORTER=otlp"}
	if off := (Agent{}).TracesOff("", env); !slices.Equal(off, []string{claudeEnhancedTelemetry, otelTracesExporter}) {
		t.Fatalf("process env: TracesOff = %q", off)
	}
	if off := (Agent{}).TracesOff("", []string{"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1", "OTEL_TRACES_EXPORTER=otlp", "HOME=/"}); len(off) != 0 {
		t.Fatalf("process env on: TracesOff = %q", off)
	}
}
