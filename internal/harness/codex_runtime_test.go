package harness

import (
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

func TestCodexRuntimeArgs(t *testing.T) {
	exp := Exporter{Endpoint: "https://otel.example.com", APIKey: "test-\"key", Signals: []Signal{SignalLogs, SignalTraces, SignalMetrics}, ResourceAttributes: map[string]string{AttrProjectID: "project-a", "user.email": "person@example.com"}}
	args := (Codex{}).RuntimeArgs(exp)
	var assignments []string
	for i := 0; i < len(args); i += 2 {
		if args[i] != "-c" {
			t.Fatalf("unexpected option %q", args[i])
		}
		assignments = append(assignments, args[i+1])
	}
	var doc struct {
		Otel struct {
			Exporter map[string]struct {
				Endpoint string
				Protocol string
				Headers  map[string]string
			}
			TraceExporter   map[string]any    `toml:"trace_exporter"`
			MetricsExporter map[string]any    `toml:"metrics_exporter"`
			Attributes      map[string]string `toml:"span_attributes"`
			LogPrompt       bool              `toml:"log_user_prompt"`
			ToolResult      struct {
				MaxBytes int `toml:"max_bytes"`
			} `toml:"tool_result"`
		}
		Analytics struct{ Enabled bool }
	}
	if err := toml.Unmarshal([]byte(strings.Join(assignments, "\n")), &doc); err != nil {
		t.Fatal(err)
	}
	http := doc.Otel.Exporter["otlp-http"]
	if http.Endpoint != exp.SignalEndpoint(SignalLogs) || http.Protocol != "binary" || http.Headers["Authorization"] != "Bearer "+exp.APIKey {
		t.Fatal("runtime exporter does not match requested endpoint/auth")
	}
	if len(doc.Otel.TraceExporter) != 1 || len(doc.Otel.MetricsExporter) != 1 || !doc.Analytics.Enabled {
		t.Fatal("missing selected signals")
	}
	if doc.Otel.Attributes[AttrProjectID] != "project-a" || doc.Otel.Attributes["user.email"] != "person@example.com" {
		t.Fatal("dotted attribute keys were not preserved")
	}
	if doc.Otel.LogPrompt || doc.Otel.ToolResult.MaxBytes != 0 {
		t.Fatal("content exclusion was lost")
	}
	exp.Signals = []Signal{SignalLogs}
	exp.IncludeToolContent = true
	args = (Codex{}).RuntimeArgs(exp)
	joined := strings.Join(args, " ")
	// Unselected signals are turned OFF explicitly, not left absent: otherwise a per-repo
	// run would fall through to whatever exporters a prior machine-wide connect left in
	// the user-level config.toml and leak this repo's telemetry to another project.
	for _, want := range []string{`otel.trace_exporter="none"`, `otel.metrics_exporter="none"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("unselected signal not turned off: want %q in\n%s", want, joined)
		}
	}
	// metrics is off, so no analytics opt-in; and no notify override on the per-repo path.
	for _, unwanted := range []string{"analytics.enabled", "notify"} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("unexpected override %s", unwanted)
		}
	}
}
