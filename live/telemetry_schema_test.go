package live

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The semantic contracts validate values and joins; these key sets catch a
// disappearing field we have not yet given a value assertion. Each baseline
// contains fields present on EVERY record of a controlled surface. Optional
// additions are reported; disappearance on even one record fails.
func checkTelemetrySchema(t *testing.T, e telemetryEvidence, harness string, exclude, newest bool) {
	t.Helper()
	shapes := map[string][]map[string]string{}
	add := func(surface string, attrs, resource map[string]string) {
		fields := maps.Clone(attrs)
		if fields == nil {
			fields = map[string]string{}
		}
		for k, v := range resource {
			fields["resource/"+k] = v
		}
		shapes[surface] = append(shapes[surface], fields)
	}
	events := []string{"user_prompt", "assistant_response", "api_request", "tool_decision", "tool_result"}
	if harness == "codex" {
		events = []string{"codex.conversation_starts", "codex.user_prompt", "codex.api_request", "codex.tool_decision", "codex.tool_result", "codex.sse_event"}
	}
	for _, r := range e.logs {
		name := r.Attrs["event.name"]
		if !slices.Contains(events, name) {
			continue
		}
		if name == "codex.sse_event" {
			if r.Attrs["event.kind"] != "response.completed" {
				continue
			}
			name += "/response.completed"
			if _, ok := r.Attrs["input_token_count"]; ok {
				name += "/usage"
			} else {
				name += "/timing"
			}
		}
		add("logs/"+name, r.Attrs, r.Resource)
	}
	for _, s := range e.spans {
		if harness == "claude" && strings.HasPrefix(s.Name, "claude_code.") {
			add("traces/"+s.Name, s.Attrs, s.Resource)
		}
		if harness == "codex" && (s.Name == "session_task.turn" || s.Attrs["gen_ai.usage.input_tokens"] != "") {
			add("traces/"+s.Name, s.Attrs, s.Resource)
		}
	}
	for _, m := range e.metrics {
		name := m.Proto.GetName()
		if harness == "claude" || slices.Contains([]string{"codex.api_request", "codex.api_request.duration_ms", "codex.tool.call", "codex.tool.call.duration_ms", "codex.turn.token_usage", "codex.turn.e2e_duration_ms", "codex.turn.ttft.duration_ms", "codex.turn.tool.call"}, name) {
			for _, p := range m.Proto.GetSum().GetDataPoints() {
				add("metrics/"+name, flatten(p.Attributes), m.Resource)
			}
			for _, p := range m.Proto.GetHistogram().GetDataPoints() {
				add("metrics/"+name, flatten(p.Attributes), m.Resource)
			}
		}
	}
	mode := map[bool]string{false: "content", true: "redacted"}[exclude]
	path := goldenPath(harness + "/telemetry-" + mode)
	if os.Getenv("LIVE_UPDATE_GOLDEN") == "1" {
		if !newest || t.Failed() {
			return
		}
		schema := map[string][]string{}
		for surface, records := range shapes {
			keys := maps.Clone(records[0])
			for _, record := range records[1:] {
				for k := range keys {
					if _, ok := record[k]; !ok {
						delete(keys, k)
					}
				}
			}
			schema[surface] = slices.Sorted(maps.Keys(keys))
		}
		data, err := json.MarshalIndent(schema, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing telemetry schema %s: %v", path, err)
	}
	var want map[string][]string
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	for surface, keys := range want {
		records := shapes[surface]
		if len(records) == 0 {
			t.Errorf("%s: no records for schema surface %s", harness, surface)
			continue
		}
		for _, record := range records {
			for _, key := range keys {
				if _, ok := record[key]; !ok {
					t.Errorf("%s: field disappeared from %s: %s", harness, surface, key)
				}
			}
		}
	}
	for surface, records := range shapes {
		additions := map[string]bool{}
		for _, record := range records {
			for k := range record {
				if !slices.Contains(want[surface], k) {
					additions[k] = true
				}
			}
		}
		if len(additions) > 0 {
			Note(harness+"/"+surface, "new fields: "+strings.Join(slices.Sorted(maps.Keys(additions)), ", "))
		}
	}
	Note(harness+"/telemetry-"+mode, "logs, traces, metrics, content policy, session/tool joins and per-export authentication checked")
}
