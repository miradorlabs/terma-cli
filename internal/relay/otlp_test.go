package relay

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

const (
	sessionA = "11111111-1111-4111-8111-111111111111"
	sessionB = "22222222-2222-4222-8222-222222222222"
	codexID  = "01a0edc9-7663-75d0-8a00-5ee4a6d0a5ef"
)

func strAttr(k, v string) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
}

func logRecord(name, trace string, attrs ...map[string]any) map[string]any {
	a := []any{strAttr("event.name", name)}
	for _, x := range attrs {
		a = append(a, x)
	}
	r := map[string]any{"body": map[string]any{"stringValue": name}, "attributes": a, "severityNumber": 9}
	if trace != "" {
		r["traceId"] = trace
	}
	return r
}

func logsBody(t *testing.T, records ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"resourceLogs": []any{map[string]any{
		"resource":  map[string]any{"attributes": []any{strAttr("service.name", "claude-code")}},
		"schemaUrl": "https://opentelemetry.io/schemas/1.26.0",
		"scopeLogs": []any{map[string]any{
			"scope":      map[string]any{"name": "com.anthropic.claude_code.events", "version": "2.1.284"},
			"logRecords": records,
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func routeBy(b *batch, f func(*item) string) {
	for _, it := range b.items() {
		it.route = f(it)
	}
}

func eventNames(t *testing.T, body []byte) []string {
	t.Helper()
	var doc struct {
		ResourceLogs []struct {
			ScopeLogs []struct {
				LogRecords []struct {
					Body struct {
						S string `json:"stringValue"`
					} `json:"body"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, rl := range doc.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, r := range sl.LogRecords {
				names = append(names, r.Body.S)
			}
		}
	}
	return names
}

func TestSplitLogsBySession(t *testing.T) {
	body := logsBody(t,
		logRecord("a1", "", strAttr("session.id", sessionA)),
		logRecord("b1", "", strAttr("session.id", sessionB)),
		logRecord("a2", "", strAttr("session.id", sessionA)),
	)
	b, err := parseBatch(sigLogs, body)
	if err != nil {
		t.Fatal(err)
	}
	routeBy(b, func(it *item) string { return map[string]string{sessionA: "proj-a", sessionB: "proj-b"}[it.session] })

	a, n, err := b.encode("proj-a")
	if err != nil || n != 2 {
		t.Fatalf("proj-a: %d items, %v", n, err)
	}
	if got := eventNames(t, a); !slices.Equal(got, []string{"a1", "a2"}) {
		t.Fatalf("proj-a records = %v, want a1 a2 in order", got)
	}
	bb, n, _ := b.encode("proj-b")
	if got := eventNames(t, bb); n != 1 || !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("proj-b records = %v", got)
	}
	// Everything the relay does not route by travels unchanged.
	for _, want := range []string{`"schemaUrl":"https://opentelemetry.io/schemas/1.26.0"`, `"version":"2.1.284"`, `"severityNumber":9`, `"service.name"`} {
		if !strings.Contains(string(a), want) {
			t.Errorf("proj-a body lost %s: %s", want, a)
		}
	}
	if none, n, _ := b.encode("proj-c"); none != nil || n != 0 {
		t.Fatalf("a route with no items encodes nothing, got %s", none)
	}
}

func TestSessionConventions(t *testing.T) {
	for _, tc := range []struct {
		attr, value, session, source string
	}{
		{"session.id", sessionA, sessionA, "claude"},
		{"conversation.id", codexID, codexID, "codex"},
		{"thread.id", codexID, codexID, "codex"},
		{"thread_id", codexID, codexID, "codex"},
		{"thread.id", "17", "", ""}, // a Codex worker-thread number, not a session
		{"session.id", "../../etc", "", ""},
	} {
		v := tc.value
		got, src := sessionFrom([]kv{{Key: tc.attr, Value: struct {
			String *string `json:"stringValue"`
		}{&v}}})
		if got != tc.session || src != tc.source {
			t.Errorf("%s=%s: got (%q, %q), want (%q, %q)", tc.attr, tc.value, got, src, tc.session, tc.source)
		}
	}
}

func TestSplitMetricsByDataPoint(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"resourceMetrics": []any{map[string]any{
		"resource": map[string]any{},
		"scopeMetrics": []any{map[string]any{"scope": map[string]any{"name": "m"}, "metrics": []any{
			map[string]any{"name": "claude_code.token.usage", "unit": "tokens", "sum": map[string]any{
				"aggregationTemporality": 1, "isMonotonic": true,
				"dataPoints": []any{
					map[string]any{"asDouble": 1, "attributes": []any{strAttr("session.id", sessionA)}},
					map[string]any{"asDouble": 2, "attributes": []any{strAttr("session.id", sessionB)}},
				}}},
			map[string]any{"name": "codex.turn.token_usage", "histogram": map[string]any{
				"dataPoints": []any{map[string]any{"count": "1"}}}},
		}}},
	}}})
	b, err := parseBatch(sigMetrics, body)
	if err != nil {
		t.Fatal(err)
	}
	routeBy(b, func(it *item) string {
		if it.session == "" {
			return machineRoute
		}
		return it.session[:1]
	})
	one, n, _ := b.encode("1")
	if n != 1 || !strings.Contains(string(one), `"asDouble":1`) || strings.Contains(string(one), `"asDouble":2`) ||
		!strings.Contains(string(one), `"aggregationTemporality":1`) || !strings.Contains(string(one), `"unit":"tokens"`) {
		t.Fatalf("session A's point, with its metric's fields: %s", one)
	}
	machine, n, _ := b.encode(machineRoute)
	if n != 1 || !strings.Contains(string(machine), "codex.turn.token_usage") || strings.Contains(string(machine), "claude_code") {
		t.Fatalf("the sessionless Codex metric alone: %s", machine)
	}
}

func TestResourceSessionApplies(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": []any{strAttr("session.id", sessionA)}},
		"scopeSpans": []any{map[string]any{"spans": []any{map[string]any{"name": "s", "traceId": "t1"}}}},
	}}})
	b, err := parseBatch(sigTraces, body)
	if err != nil {
		t.Fatal(err)
	}
	if it := b.items()[0]; it.session != sessionA || it.traceID != "t1" {
		t.Fatalf("item = %+v", it)
	}
}

func TestParseRejectsNonOTLP(t *testing.T) {
	for _, body := range []string{`not json`, `{"resourceLogs": 3}`, `{"resourceLogs":[{"scopeLogs":[{"logRecords":[1]}]}]}`} {
		if _, err := parseBatch(sigLogs, []byte(body)); err == nil {
			t.Errorf("%s parsed", body)
		}
	}
	if b, err := parseBatch(sigLogs, []byte(`{}`)); err != nil || len(b.items()) != 0 {
		t.Errorf("an empty export is valid and empty: %v", err)
	}
}

func TestMergeBodies(t *testing.T) {
	one := logsBody(t, logRecord("x", "", strAttr("session.id", sessionA)))
	two := logsBody(t, logRecord("y", "", strAttr("session.id", sessionA)))
	merged, err := mergeBodies(sigLogs, [][]byte{one, two})
	if err != nil {
		t.Fatal(err)
	}
	if got := eventNames(t, merged); !slices.Equal(got, []string{"x", "y"}) {
		t.Fatalf("merged = %v", got)
	}
}
