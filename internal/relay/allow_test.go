package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Every key the live goldens record with content withheld is classified here.
func TestClassificationCoversTheGoldens(t *testing.T) {
	t.Parallel()
	files, _ := filepath.Glob(filepath.Join("..", "..", "test", "e2e", "golden", "*", "telemetry-redacted.json"))
	withheld, _ := filepath.Glob(filepath.Join("..", "..", "test", "e2e", "golden", "relay", "*-withheld.json"))
	files = append(files, withheld...)
	if len(files) == 0 {
		t.Fatal("no goldens found")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var surfaces map[string][]string
		if err := json.Unmarshal(data, &surfaces); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for surface, keys := range surfaces {
			for _, k := range keys {
				// The goldens name a resource attribute "resource/<key>".
				q := FieldQuery{Site: SiteRecord, Key: k}
				if key, ok := strings.CutPrefix(k, "resource/"); ok {
					q = FieldQuery{Site: SiteResource, Key: key}
				}
				if testRules.classify(q) == FieldUnclassified {
					t.Errorf("%s %s: %q is unclassified", filepath.Base(f), surface, k)
				}
			}
		}
	}
}

// A key is safe or content, never both, on a metric alone too.
func TestNoKeyIsBothSafeAndContent(t *testing.T) {
	t.Parallel()
	for key := range testRules.safeKeys {
		if testRules.contentKey(key) {
			t.Errorf("%q is listed as safe and as content", key)
		}
	}
	for metric, keys := range testRules.metricSafeKeys {
		for _, key := range keys {
			if testRules.contentKey(key) {
				t.Errorf("%q is listed as safe on %s and as content", key, metric)
			}
		}
	}
}

// With content withheld, unknown attributes and free-text bodies are dropped and counted;
// with content allowed, nothing is touched.
func TestWithheldContentPassesOnlyWhatIsClassified(t *testing.T) {
	t.Parallel()
	record := func() *part {
		return &part{signal: Logs, session: "A", msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "x"), kv("process.command_args", "-p secret"), kv("host.fancy", "new")}},
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
				{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "the prompt, in a new place"}},
					Attributes: []*commonpb.KeyValue{kv("event.name", "x.new_event"), kv("session.id", "A"), kv("input_tokens", "3"), kv("x.new_text", "secret")}},
				{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{kv("text", "secret")}}}},
					Attributes: []*commonpb.KeyValue{kv("event.name", "x.structured")}},
				{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.api_request"}},
					Attributes: []*commonpb.KeyValue{kv("event.name", "api_request")}},
			}}},
		}}}}
	}
	unclassified := map[string]int{}
	p := record()
	testRules.withhold(p, false, false, unclassified)
	rl := p.msg.(*logspb.LogsData).ResourceLogs[0]
	recs := rl.ScopeLogs[0].LogRecords
	for _, want := range []string{"x.new_text", "resource/host.fancy"} {
		if unclassified[want] == 0 {
			t.Errorf("%s was not counted as unclassified: %v", want, unclassified)
		}
	}
	if attr(recs[0].Attributes, "x.new_text") != "" || attr(recs[0].Attributes, "input_tokens") != "3" {
		t.Errorf("attributes after the gate: %v", recs[0].Attributes)
	}
	if attr(rl.Resource.Attributes, "process.command_args") != "" || attr(rl.Resource.Attributes, "host.fancy") != "" || attr(rl.Resource.Attributes, "service.name") != "x" {
		t.Errorf("resource after the gate: %v", rl.Resource.Attributes)
	}
	if recs[0].Body.GetStringValue() != "" || recs[1].Body.GetKvlistValue() != nil {
		t.Errorf("a body that says more than its event left: %v / %v", recs[0].Body, recs[1].Body)
	}
	if recs[2].Body.GetStringValue() != "claude_code.api_request" {
		t.Errorf("a body that only names its event was blanked: %v", recs[2].Body)
	}

	unclassified = map[string]int{}
	p = record()
	if n := testRules.withhold(p, true, true, unclassified); n != 0 || len(unclassified) != 0 {
		t.Fatalf("content allowed, yet the gate changed %d records: %v", n, unclassified)
	}
}

// Claude Code's login ids and settings pass whatever content is withheld: the platform stamps a
// call's funding from user.account_uuid. The email is personal and stays behind.
func TestWithheldContentKeepsClaudeAccountIDs(t *testing.T) {
	kept := map[string]string{"user.account_uuid": "4f9c", "user.account_id": "user_01A", "organization.id": "0e5b",
		"client_request_id": "7d2a", "effort": "high", "agent.name": "Explore"}
	record := func(event string, extra ...*commonpb.KeyValue) *logspb.LogRecord {
		attrs := []*commonpb.KeyValue{kv("event.name", event), kv("user.email", "dev@example.com")}
		for k, v := range kept {
			attrs = append(attrs, kv(k, v))
		}
		return &logspb.LogRecord{Attributes: append(attrs, extra...)}
	}
	for _, flags := range [][2]bool{{false, false}, {false, true}, {true, false}} {
		prompts, toolContent := flags[0], flags[1]
		recs := []*logspb.LogRecord{record("api_request"), record("user_prompt", kv("prompt", "PRIVATE")),
			record("tool_result", kv("tool_parameters", "PRIVATE"))}
		unclassified := map[string]int{}
		testRules.withhold(&part{signal: Logs, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}}}}}}, prompts, toolContent, unclassified)
		for _, r := range recs {
			for k, v := range kept {
				if attr(r.Attributes, k) != v {
					t.Errorf("prompts=%v toolContent=%v: %s lost %s", prompts, toolContent, attr(r.Attributes, "event.name"), k)
				}
			}
			if attr(r.Attributes, "user.email") != "" {
				t.Errorf("prompts=%v toolContent=%v: user.email left", prompts, toolContent)
			}
		}
		if unclassified["user.email"] != len(recs) {
			t.Errorf("prompts=%v toolContent=%v: unclassified %v", prompts, toolContent, unclassified)
		}
		if got := attr(recs[1].Attributes, "prompt"); (got == "PRIVATE") != prompts {
			t.Errorf("prompts=%v: prompt is %q", prompts, got)
		}
		if got := attr(recs[2].Attributes, "tool_parameters"); (got == "PRIVATE") != toolContent {
			t.Errorf("toolContent=%v: tool_parameters is %q", toolContent, got)
		}
	}
}

// With the email withheld, a Codex record carries the backend's seat key in its place: the
// golden is mirador-platform's scopedPrincipalID for this workspace and login. A Claude record,
// keyed on user.account_uuid, gains nothing.
func TestWithheldEmailLeavesCodexPrincipal(t *testing.T) {
	const golden = "5902e6b5f0ef205a210b4e40a2bcbdb5d2cffd3d32e2bf818d631629cde6bdd8"
	login := func(extra ...*commonpb.KeyValue) *logspb.LogRecord {
		return &logspb.LogRecord{Attributes: append([]*commonpb.KeyValue{kv("event.name", "codex.sse_event"),
			kv("user.account_id", "00000000-0000-4000-8000-000000000001"), kv("user.email", "user@example.com")}, extra...)}
	}
	codex, claude := login(kv("conversation.id", "01a108fc")), login(kv("session.id", "863385f4"))
	p := &part{signal: Logs, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{codex, claude}}}}}}}
	// Twice: a queued export is withheld again when it is sent.
	for range 2 {
		testRules.withhold(p, false, true, map[string]int{})
	}
	var ids []string
	for _, a := range codex.Attributes {
		if a.GetKey() == semconv.TermaAccountSeatIDKey {
			ids = append(ids, a.GetValue().GetStringValue())
		}
	}
	if len(ids) != 1 || ids[0] != golden || attr(codex.Attributes, "user.email") != "" {
		t.Errorf("codex principal %q, email %q", ids, attr(codex.Attributes, "user.email"))
	}
	if attr(claude.Attributes, semconv.TermaAccountSeatIDKey) != "" {
		t.Errorf("claude record gained %s", semconv.TermaAccountSeatIDKey)
	}
}

// Codex sends some metric points at one timestamp told apart only by the MCP server they
// name; dropping that attribute would merge them and the backend would keep one (#127).
func TestWithheldContentKeepsCodexMCPSeriesApart(t *testing.T) {
	t.Parallel()
	for _, flags := range [][2]bool{{false, false}, {false, true}, {true, false}} {
		prompts, toolContent := flags[0], flags[1]
		points := []*metricspb.NumberDataPoint{
			{TimeUnixNano: 1, Attributes: []*commonpb.KeyValue{kv("mode", "auto"), kv("outcome", "modern")}},
			{TimeUnixNano: 1, Attributes: []*commonpb.KeyValue{kv("mode", "auto"), kv("outcome", "modern"), kv("server_kind", "openai_codex_apps")}},
			{TimeUnixNano: 1, Attributes: []*commonpb.KeyValue{kv("outcome", "found"), kv("server_name", "docs")}},
			{TimeUnixNano: 1, Attributes: []*commonpb.KeyValue{kv("outcome", "found"), kv("server_name", "github")}},
		}
		unclassified := map[string]int{}
		testRules.withhold(&part{signal: Metrics, msg: &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
				{Name: "codex.mcp.protocol_discovery", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: points[:2]}}},
				{Name: "codex.mcp.executor_discovery.server", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: points[2:]}}},
			}}}}}}}, prompts, toolContent, unclassified)
		if attr(points[1].Attributes, "server_kind") != "openai_codex_apps" ||
			attr(points[2].Attributes, "server_name") != "docs" || attr(points[3].Attributes, "server_name") != "github" {
			t.Errorf("prompts=%v toolContent=%v: points merged: %v, unclassified %v", prompts, toolContent, points, unclassified)
		}
	}
}

// Codex sends its rollout persistence metrics for 1% of threads, so a missing key fails
// only the runs that land on a sampled thread.
func TestWithheldContentKeepsCodexRolloutPersistenceKeys(t *testing.T) {
	t.Parallel()
	for _, flags := range [][2]bool{{false, false}, {false, true}, {true, false}} {
		prompts, toolContent := flags[0], flags[1]
		points := []*metricspb.HistogramDataPoint{
			{Attributes: []*commonpb.KeyValue{kv("decision", "persisted"), kv("rollout_item_type", "response.message"),
				kv("encoding", "rollout_item_json_v1"), kv("sample_rate", "0.01")}},
			{Attributes: []*commonpb.KeyValue{kv("stage", "pre_filter"), kv("outcome", "completed"),
				kv("encoding", "rollout_item_json_v1"), kv("sample_rate", "0.01")}},
		}
		unclassified := map[string]int{}
		testRules.withhold(&part{signal: Metrics, msg: &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
				{Name: "codex.rollout.persistence.item_bytes", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: points[:1]}}},
				{Name: "codex.rollout.persistence.turn_bytes", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: points[1:]}}},
			}}}}}}}, prompts, toolContent, unclassified)
		if len(unclassified) != 0 || attr(points[0].Attributes, "rollout_item_type") != "response.message" ||
			attr(points[0].Attributes, "encoding") != "rollout_item_json_v1" || attr(points[1].Attributes, "encoding") != "rollout_item_json_v1" {
			t.Errorf("prompts=%v toolContent=%v: points %v, unclassified %v", prompts, toolContent, points, unclassified)
		}
	}
}

// A number or flag passes under any key, even as a string that is wholly one; anything more is text.
func TestNumbersAndFlagsPassUnderAnyKey(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{"3": true, "-12.5": true, "true": true, "false": true, "0": true,
		"": false, "3 files": false, "0x1f": false, "NaN": false, "Inf": false, "1_000": false, "yes": false, "1e999999": false} {
		if got := numericOrBool(v); got != want {
			t.Errorf("numericOrBool(%q) = %v, want %v", v, got, want)
		}
	}
	unclassified := map[string]int{}
	attrs, _ := testRules.withholdAttrs([]*commonpb.KeyValue{kv("num_hooks", "3"), kv("x.new_note", "3 files"),
		{Key: "x.count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 4}}}}, false, false, false, nil, unclassified)
	if attr(attrs, "num_hooks") != "3" || len(attrs) != 2 || unclassified["x.new_note"] != 1 {
		t.Fatalf("attrs %v, unclassified %v", attrs, unclassified)
	}
}

// A tool-content event the policy sends keeps its output whatever the key: Claude's Read
// tool puts it in `content` on tool.output, which no rule names (#126). Prompt keys in it are
// still withheld, an unclassified key outside it is still dropped, and with tool content
// withheld the event goes.
func TestToolContentEventsKeepTheirOutput(t *testing.T) {
	traces := func() *part {
		sp := &tracepb.Span{Name: "tool", Attributes: []*commonpb.KeyValue{kv("content", "span-level text")},
			Events: []*tracepb.Span_Event{{Name: "tool.output", Attributes: []*commonpb.KeyValue{kv("content", "READ OUTPUT"), kv("tool_name", "Read"),
				kv("prompt", "what was asked"), kv("gen_ai.prompt", "what was asked")}}}}
		return &part{signal: Traces, msg: &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}}}}
	}
	span := func(p *part) *tracepb.Span {
		return p.msg.(*tracepb.TracesData).ResourceSpans[0].ScopeSpans[0].Spans[0]
	}

	// Prompts withheld, tool content sent: the Read output stays; its prompt keys and a
	// span-level `content` do not.
	p := traces()
	testRules.withhold(p, false, true, map[string]int{})
	sp := span(p)
	if len(sp.Events) != 1 || attr(sp.Events[0].Attributes, "content") != "READ OUTPUT" {
		t.Fatalf("tool.output after withholding prompts: %v", sp.Events)
	}
	if ev := sp.Events[0].Attributes; attr(ev, "prompt") != defaultMarker || attr(ev, "gen_ai.prompt") != "" {
		t.Fatalf("a prompt passed inside tool.output: %v", ev)
	}
	if attr(sp.Attributes, "content") != "" {
		t.Fatalf("an unclassified span attribute passed: %v", sp.Attributes)
	}

	// Tool content withheld: the event goes whole.
	p = traces()
	testRules.withhold(p, true, false, map[string]int{})
	if events := span(p).Events; len(events) != 0 {
		t.Fatalf("tool.output survived withheld tool content: %v", events)
	}
}

// terma.session.historical is non-content metadata: it leaves intact on both resource and
// log record attributes under every withholding policy (as boolean true or string "true"),
// while withheld prompts, replies, tool content, and unclassified text stay off the wire.
func TestWithheldContentKeepsHistoricalSessionMarker(t *testing.T) {
	t.Parallel()
	values := []struct {
		name string
		val  *commonpb.AnyValue
	}{
		{"bool", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
		{"string", strValue("true")},
	}
	findAttr := func(attrs []*commonpb.KeyValue, key string) *commonpb.AnyValue {
		for _, a := range attrs {
			if a.GetKey() == key {
				return a.GetValue()
			}
		}
		return nil
	}
	for _, vc := range values {
		for _, flags := range [][2]bool{{false, false}, {false, true}, {true, false}, {true, true}} {
			prompts, toolContent := flags[0], flags[1]
			resHist := &commonpb.KeyValue{Key: semconv.TermaSessionHistoricalKey, Value: proto.CloneOf(vc.val)}
			recHist := &commonpb.KeyValue{Key: semconv.TermaSessionHistoricalKey, Value: proto.CloneOf(vc.val)}
			msg := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
				Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
					kv("service.name", "claude-code"),
					resHist,
					kv("process.command_args", "SECRET-ARGS"),
					kv("cwd", "/Users/dev/SECRET-CWD"),
					kv("host.secret", "SECRET-HOST"),
				}},
				ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
					Body: strValue("SECRET-BODY"),
					Attributes: []*commonpb.KeyValue{
						kv("event.name", "user_prompt"),
						kv("session.id", "hist-1"),
						recHist,
						kv("prompt", "SECRET-PROMPT"),
						kv("gen_ai.prompt", "SECRET-GENAI-PROMPT"),
						kv("response", "SECRET-REPLY"),
						kv("gen_ai.completion", "SECRET-COMPLETION"),
						kv("tool_parameters", "SECRET-TOOL-PARAMS"),
						kv("gen_ai.tool.call.arguments", "SECRET-TOOL-ARGS"),
						kv("gen_ai.tool.call.result", "SECRET-TOOL-RESULT"),
						kv("x.unknown", "SECRET-UNKNOWN"),
					},
				}}}},
			}}}
			unclassified := map[string]int{}
			testRules.withhold(&part{signal: Logs, session: "hist-1", msg: msg}, prompts, toolContent, unclassified)

			rl := msg.ResourceLogs[0]
			rec := rl.ScopeLogs[0].LogRecords[0]
			if got := findAttr(rl.Resource.Attributes, semconv.TermaSessionHistoricalKey); !proto.Equal(got, vc.val) {
				t.Errorf("%s prompts=%v tools=%v: resource %s = %v, want %v", vc.name, prompts, toolContent, semconv.TermaSessionHistoricalKey, got, vc.val)
			}
			if got := findAttr(rec.Attributes, semconv.TermaSessionHistoricalKey); !proto.Equal(got, vc.val) {
				t.Errorf("%s prompts=%v tools=%v: record %s = %v, want %v", vc.name, prompts, toolContent, semconv.TermaSessionHistoricalKey, got, vc.val)
			}
			if unclassified[semconv.TermaSessionHistoricalKey] != 0 || unclassified["resource/"+semconv.TermaSessionHistoricalKey] != 0 {
				t.Errorf("%s prompts=%v tools=%v: historical marker counted unclassified: %v", vc.name, prompts, toolContent, unclassified)
			}

			wire, err := proto.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			text := string(wire)
			for _, secret := range []string{"SECRET-ARGS", "SECRET-PROMPT", "SECRET-GENAI-PROMPT", "SECRET-REPLY", "SECRET-COMPLETION"} {
				if leaked := strings.Contains(text, secret); leaked != prompts {
					t.Errorf("%s prompts=%v tools=%v: %s on wire = %v, want %v", vc.name, prompts, toolContent, secret, leaked, prompts)
				}
			}
			for _, secret := range []string{"SECRET-CWD", "SECRET-TOOL-PARAMS", "SECRET-TOOL-ARGS", "SECRET-TOOL-RESULT"} {
				if leaked := strings.Contains(text, secret); leaked != toolContent {
					t.Errorf("%s prompts=%v tools=%v: %s on wire = %v, want %v", vc.name, prompts, toolContent, secret, leaked, toolContent)
				}
			}
			both := prompts && toolContent
			for _, secret := range []string{"SECRET-HOST", "SECRET-BODY", "SECRET-UNKNOWN"} {
				if leaked := strings.Contains(text, secret); leaked != both {
					t.Errorf("%s prompts=%v tools=%v: %s on wire = %v, want %v", vc.name, prompts, toolContent, secret, leaked, both)
				}
			}
		}
	}

	verdicts := Classify(testCapturers, []FieldQuery{
		{Site: SiteResource, Key: semconv.TermaSessionHistoricalKey},
		{Site: SiteRecord, Key: semconv.TermaSessionHistoricalKey},
	})
	for i, site := range []FieldSite{SiteResource, SiteRecord} {
		if verdicts[i].Class != FieldSafe {
			t.Errorf("Classify(%s, %s) = %+v, want %s", site, semconv.TermaSessionHistoricalKey, verdicts[i], FieldSafe)
		}
	}
}

// ContentFilter.FilterLogs (used by `terma history upload`, which bypasses the live relay
// daemon): under a restrictive policy, withheld prompt, reply, and tool content stay off
// the wire while terma.session.historical=true survives on both resource and log record
// attributes with zero unclassified keys.
func TestContentFilterPreservesHistoricalSessionMarkerUnderRestrictivePolicy(t *testing.T) {
	t.Parallel()
	filter := NewContentFilter(testCapturers)
	boolTrue := &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}
	msg := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			kv("service.name", "claude-code"),
			{Key: semconv.TermaSessionHistoricalKey, Value: proto.CloneOf(boolTrue)},
			kv("process.command_args", "SECRET-ARGS"),
			kv("cwd", "/work/SECRET-CWD"),
		}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			Body: strValue("SECRET-BODY"),
			Attributes: []*commonpb.KeyValue{
				kv("event.name", "user_prompt"),
				kv("session.id", "A"),
				{Key: semconv.TermaSessionHistoricalKey, Value: proto.CloneOf(boolTrue)},
				kv("prompt", "SECRET-PROMPT"),
				kv("response", "SECRET-REPLY"),
				kv("gen_ai.tool.call.arguments", "SECRET-TOOL-ARGS"),
				kv("gen_ai.tool.call.result", "SECRET-TOOL-RESULT"),
			},
		}}}},
	}}}
	changed, unclassified := filter.FilterLogs(msg, false, false)
	if changed == 0 {
		t.Fatal("expected restrictive policy to withhold content")
	}
	if len(unclassified) != 0 {
		t.Fatalf("unexpected unclassified keys: %v", unclassified)
	}
	wire, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "SECRET-") {
		t.Fatalf("withheld content survived filter: %q", string(wire))
	}
	rl := msg.ResourceLogs[0]
	rec := rl.ScopeLogs[0].LogRecords[0]
	checkBoolTrue := func(where string, attrs []*commonpb.KeyValue) {
		t.Helper()
		for _, a := range attrs {
			if a.GetKey() == semconv.TermaSessionHistoricalKey {
				b, ok := a.GetValue().GetValue().(*commonpb.AnyValue_BoolValue)
				if !ok || !b.BoolValue {
					t.Errorf("%s %s = %v, want bool true", where, semconv.TermaSessionHistoricalKey, a.GetValue())
				}
				return
			}
		}
		t.Errorf("%s missing %s", where, semconv.TermaSessionHistoricalKey)
	}
	checkBoolTrue("resource", rl.Resource.Attributes)
	checkBoolTrue("log record", rec.Attributes)
}

// End-to-end through the live relay daemon: live exports strip any agent-supplied
// terma.session.historical attribute so a live process cannot spoof historical sessions
// (history uploads bypass the live relay daemon and use ContentFilter directly).
func TestRelayStripsHistoricalSessionMarkerOnLiveExports(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	r, srv := f.relay(t, u, map[string]Policy{
		"p1": {Endpoint: u.srv.URL, Key: "key-p1", IncludePrompts: false, IncludeToolContent: false},
	})
	boolTrue := &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}
	msg := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			kv("service.name", "claude-code"),
			{Key: semconv.TermaSessionHistoricalKey, Value: proto.CloneOf(boolTrue)},
			kv("process.command_args", "SECRET-ARGS"),
			kv("cwd", "/work/SECRET-CWD"),
		}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			Body: strValue("SECRET-BODY"),
			Attributes: []*commonpb.KeyValue{
				kv("event.name", "user_prompt"),
				kv("session.id", "A"),
				{Key: semconv.TermaSessionHistoricalKey, Value: proto.CloneOf(boolTrue)},
				kv("prompt", "SECRET-PROMPT"),
				kv("response", "SECRET-REPLY"),
				kv("gen_ai.tool.call.arguments", "SECRET-TOOL-ARGS"),
				kv("gen_ai.tool.call.result", "SECRET-TOOL-RESULT"),
			},
		}}}},
	}}}
	postProto(t, srv, "/v1/logs", msg)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })

	u.mu.Lock()
	var sent []byte
	for _, req := range u.requests {
		if req.path == "/v1/logs" && req.auth == "Bearer key-p1" {
			sent = req.body
		}
	}
	u.mu.Unlock()
	if len(sent) == 0 {
		t.Fatal("no log export reached upstream")
	}
	if strings.Contains(string(sent), "SECRET-") {
		t.Fatalf("withheld content reached upstream: %q", string(sent))
	}
	var got logspb.LogsData
	if err := proto.Unmarshal(sent, &got); err != nil {
		t.Fatal(err)
	}
	rl := got.ResourceLogs[0]
	rec := rl.ScopeLogs[0].LogRecords[0]
	hasKey := func(attrs []*commonpb.KeyValue, key string) bool {
		for _, a := range attrs {
			if a.GetKey() == key {
				return true
			}
		}
		return false
	}
	if hasKey(rl.Resource.Attributes, semconv.TermaSessionHistoricalKey) {
		t.Errorf("live relay left %s on resource", semconv.TermaSessionHistoricalKey)
	}
	if hasKey(rec.Attributes, semconv.TermaSessionHistoricalKey) {
		t.Errorf("live relay left %s on log record", semconv.TermaSessionHistoricalKey)
	}
	for name := range r.Stats().Snapshot().Counters {
		if strings.Contains(name, semconv.TermaSessionHistoricalKey) {
			t.Errorf("historical marker counted in relay stats: %s", name)
		}
	}
}

// ContentFilter.FilterTraces preserves terma.session.historical=true and
// terma.session.parent.id on both resources and spans under restrictive team policies
// while stripping prompt, reply, system instructions, and tool content with zero
// unclassified keys.
func TestContentFilterTracesPreservesHistoricalAndParentAndRedactsContent(t *testing.T) {
	t.Parallel()
	filter := &ContentFilter{rules: testRules}
	boolTrue := &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}
	msg := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			kv("service.name", "oh-my-pi"),
			kv("service.version", "19.0.1"),
			{Key: semconv.TermaSessionHistoricalKey, Value: proto.CloneOf(boolTrue)},
			kv(semconv.TermaSessionParentIDKey, "parent-sess-1"),
			kv("process.command_args", "SECRET-ARGS"),
			kv("cwd", "/work/SECRET-CWD"),
		}},
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope: &commonpb.InstrumentationScope{Name: "@oh-my-pi/pi-agent-core"},
			Spans: []*tracepb.Span{
				{
					Name: "chat claude-sonnet-4-6",
					Attributes: []*commonpb.KeyValue{
						kv("gen_ai.operation.name", "chat"),
						kv("gen_ai.conversation.id", "sub-sess-1"),
						kv("gen_ai.request.model", "claude-sonnet-4-6"),
						kv("gen_ai.provider.name", "anthropic"),
						kv("gen_ai.agent.id", "0-explore"),
						kv("gen_ai.agent.name", "explore"),
						{Key: semconv.TermaSessionHistoricalKey, Value: proto.CloneOf(boolTrue)},
						kv("gen_ai.system_instructions", `[{"type":"text","content":"SECRET-SYS"}]`),
						kv("gen_ai.input.messages", `[{"role":"user","parts":[{"type":"text","content":"SECRET-PROMPT"}]}]`),
						kv("gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"text","content":"SECRET-REPLY"}]}]`),
					},
				},
				{
					Name: "execute_tool read",
					Attributes: []*commonpb.KeyValue{
						kv("gen_ai.operation.name", "execute_tool"),
						kv("gen_ai.conversation.id", "sub-sess-1"),
						kv("gen_ai.tool.name", "read"),
						kv("gen_ai.tool.call.id", "call-1"),
						kv("gen_ai.tool.type", "function"),
						kv("omp.gen_ai.tool.status", "error"),
						kv("error.type", "error"),
						{Key: semconv.TermaSessionHistoricalKey, Value: proto.CloneOf(boolTrue)},
						kv("gen_ai.tool.call.arguments", `{"path":"SECRET-PATH"}`),
						kv("gen_ai.tool.call.result", `{"content":[{"type":"text","text":"SECRET-OUTPUT"}]}`),
					},
				},
			},
		}},
	}}}

	changed, unclassified := filter.FilterTraces(msg, false, false)
	if changed == 0 {
		t.Fatal("expected restrictive policy to withhold trace content")
	}
	if len(unclassified) != 0 {
		t.Fatalf("unexpected unclassified keys: %v", unclassified)
	}
	wire, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "SECRET-") {
		t.Fatalf("withheld trace content survived FilterTraces: %q", string(wire))
	}
	rs := msg.ResourceSpans[0]
	checkBoolTrue := func(where string, attrs []*commonpb.KeyValue) {
		t.Helper()
		for _, a := range attrs {
			if a.GetKey() == semconv.TermaSessionHistoricalKey {
				b, ok := a.GetValue().GetValue().(*commonpb.AnyValue_BoolValue)
				if !ok || !b.BoolValue {
					t.Errorf("%s %s = %v, want bool true", where, semconv.TermaSessionHistoricalKey, a.GetValue())
				}
				return
			}
		}
		t.Errorf("%s missing %s", where, semconv.TermaSessionHistoricalKey)
	}
	checkBoolTrue("resource", rs.Resource.Attributes)
	if got := attr(rs.Resource.Attributes, semconv.TermaSessionParentIDKey); got != "parent-sess-1" {
		t.Errorf("resource %s = %q, want parent-sess-1", semconv.TermaSessionParentIDKey, got)
	}
	for i, sp := range rs.ScopeSpans[0].Spans {
		checkBoolTrue("span", sp.Attributes)
		if attr(sp.Attributes, "gen_ai.conversation.id") != "sub-sess-1" {
			t.Errorf("span %d lost gen_ai.conversation.id", i)
		}
	}
}
