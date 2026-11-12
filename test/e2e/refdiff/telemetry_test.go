package refdiff

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// telemetrySnapshot is a build whose telemetry section is doc.
func telemetrySnapshot(t *testing.T, version, doc string) *Snapshot {
	t.Helper()
	var tel map[string]any
	if err := json.Unmarshal([]byte(doc), &tel); err != nil {
		t.Fatal(err)
	}
	return &Snapshot{Harness: "codex", Version: version, Telemetry: tel}
}

func telemetryTitles(a, b *Snapshot) []string {
	var out []string
	for _, c := range New(a, b, nil, time.Now()).Changes {
		if c.Category == "telemetry" {
			out = append(out, c.Title)
		}
	}
	return out
}

const (
	// census is a nightly's capture: every surface with its keys, and the probe's findings.
	census = `{"export":{"protocol":"http/protobuf","interval_ms":{"logs":1000},"interval_batches":{"logs":1}},
	  "surfaces":{"logs/codex.api_request":{"keys":{"model":{"kinds":["text"],"class":"safe"}}},
	  "metrics/codex.turns":{"keys":{},"type":"sum","monotonic":true}}}`
	// censusOnly predates the probe: keys, no export, no metric types.
	censusOnly = `{"surfaces":{"logs/codex.api_request":{"keys":{"model":{"kinds":["text"],"class":"safe"}}},
	  "metrics/codex.turns":{"keys":{}}}}`
	// probeOnly is the frequent check's capture: the probe's findings, fewer surfaces, no keys.
	probeOnly = `{"export":{"protocol":"http/protobuf","interval_ms":{"logs":500},"interval_batches":{"logs":1}},
	  "surfaces":{"metrics/codex.turns":{"type":"sum","monotonic":true}}}`
)

// A part of the telemetry one build's capture did not measure is not observed there: a capture
// that ran the probe alone sends no surface "no longer", and the first to run it adds no export.
func TestTelemetryComparesWhatBothMeasured(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"census to probe alone", census, probeOnly},
		{"probe alone to census", probeOnly, census},
		{"before the probe to census and probe", censusOnly, census},
	} {
		if got := telemetryTitles(telemetrySnapshot(t, "1", tc.a), telemetrySnapshot(t, "2", tc.b)); len(got) > 0 {
			t.Errorf("%s: %q", tc.name, got)
		}
	}
	// What both measured is still compared.
	gone := strings.Replace(census, `"logs/codex.api_request":{"keys":{"model":{"kinds":["text"],"class":"safe"}}},`, "", 1)
	if got := telemetryTitles(telemetrySnapshot(t, "1", census), telemetrySnapshot(t, "2", gone)); len(got) != 1 || !strings.Contains(got[0], "logs/codex.api_request") {
		t.Errorf("a surface both censuses measured, gone: %q", got)
	}
	unit := strings.Replace(probeOnly, `"monotonic":true`, `"monotonic":false`, 1)
	if got := telemetryTitles(telemetrySnapshot(t, "1", probeOnly), telemetrySnapshot(t, "2", unit)); len(got) != 1 {
		t.Errorf("a probe finding both probes measured, changed: %q", got)
	}
	newKey := strings.Replace(censusOnly, `"keys":{}`, `"keys":{"turn":{"kinds":["int"],"class":"safe"}}`, 1)
	if got := telemetryTitles(telemetrySnapshot(t, "1", census), telemetrySnapshot(t, "2", newKey)); len(got) != 1 {
		t.Errorf("a key both censuses measured, new, beside a probe one did not run: %q", got)
	}
}

// An export interval rests on the batches the run saw: from one, it is the run's jitter, and how
// many there were is the run's; from several on both sides, a new interval is a change.
func TestExportIntervalNeedsSeveralBatches(t *testing.T) {
	const noInterval = `{"export":{"protocol":"http/protobuf"},"surfaces":{"metrics/m":{"type":"sum"}}}`
	export := func(ms, batches int) string {
		b, _ := json.Marshal(map[string]any{"export": map[string]any{"protocol": "http/protobuf",
			"interval_ms": map[string]int{"logs": ms}, "interval_batches": map[string]int{"logs": batches}},
			"surfaces": map[string]any{"metrics/m": map[string]any{"type": "sum"}}})
		return string(b)
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{export(1000, 1), export(500, 1), 0},
		{export(1000, 3), export(500, 1), 0},
		{export(1000, 3), export(1000, 5), 0},
		{export(1000, 3), export(500, 4), 1},
		// A run that caught no batch before the cutoff measured no interval at all.
		{export(1000, 1), noInterval, 0},
		{noInterval, export(1000, 1), 0},
		{export(1000, 3), noInterval, 0},
	} {
		if got := telemetryTitles(telemetrySnapshot(t, "1", tc.a), telemetrySnapshot(t, "2", tc.b)); len(got) != tc.want {
			t.Errorf("%s → %s: %q, want %d changes", tc.a, tc.b, got, tc.want)
		}
	}
}

// A probe that saw logs alone measured the export, though no surface has a probe field: its
// findings are compared with another probe's, and a census beside it measured none of them.
func TestLogsOnlyProbeMeasuredTheExport(t *testing.T) {
	logsProbe := `{"export":{"protocol":"http/protobuf"},"surfaces":{"logs/codex.api_request":{}}}`
	var tel map[string]any
	if err := json.Unmarshal([]byte(logsProbe), &tel); err != nil {
		t.Fatal(err)
	}
	if census, probe := TelemetryMeasured(tel); census || !probe {
		t.Errorf("TelemetryMeasured = census %v, probe %v; want the probe alone", census, probe)
	}
	asJSON := strings.Replace(logsProbe, "http/protobuf", "http/json", 1)
	if got := telemetryTitles(telemetrySnapshot(t, "1", logsProbe), telemetrySnapshot(t, "2", asJSON)); len(got) != 1 {
		t.Errorf("an export both probes measured, changed: %q", got)
	}
}
