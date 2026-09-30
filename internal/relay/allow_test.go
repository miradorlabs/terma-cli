package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// Every key a harness was seen to send with content withheld is classified: the
// goldens the live suite records are the evidence, and a key they gain on a new release
// must be decided here before the relay lets it through.
func TestClassificationCoversTheGoldens(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "live", "golden", "*", "telemetry-redacted.json"))
	withheld, _ := filepath.Glob(filepath.Join("..", "..", "live", "golden", "relay", "*-withheld.json"))
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
				if c := Classify(strings.TrimPrefix(k, "resource/")); c == "unclassified" {
					t.Errorf("%s %s: %q is unclassified", filepath.Base(f), surface, k)
				}
			}
		}
	}
}

// A key is safe or content, never both: a content key listed as safe would pass whatever
// the gate did to it.
func TestNoKeyIsBothSafeAndContent(t *testing.T) {
	for key := range safeKeys {
		if contentKey(key) {
			t.Errorf("%q is listed as safe and as content", key)
		}
	}
}

// With content withheld, what is not known to be safe does not leave: an unknown
// attribute, an unknown resource attribute, and a body that says more than its event's
// name are dropped and counted by name. With content allowed, nothing is touched.
func TestWithheldContentPassesOnlyWhatIsClassified(t *testing.T) {
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
	withhold(p, false, false, unclassified)
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
	if n := withhold(p, true, true, unclassified); n != 0 || len(unclassified) != 0 {
		t.Fatalf("content allowed, yet the gate changed %d records: %v", n, unclassified)
	}
}

// A count or a flag cannot carry what was said, whatever its key, sent as a number or
// as a string that is wholly one; anything more is text and must be classified.
func TestNumbersAndFlagsPassUnderAnyKey(t *testing.T) {
	for v, want := range map[string]bool{"3": true, "-12.5": true, "true": true, "false": true, "0": true,
		"": false, "3 files": false, "0x1f": false, "NaN": false, "Inf": false, "1_000": false, "yes": false, "1e999999": false} {
		if got := numericOrBool(v); got != want {
			t.Errorf("numericOrBool(%q) = %v, want %v", v, got, want)
		}
	}
	unclassified := map[string]int{}
	attrs, _ := withholdAttrs([]*commonpb.KeyValue{kv("num_hooks", "3"), kv("x.new_note", "3 files"),
		{Key: "x.count", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 4}}}}, false, false, unclassified)
	if attr(attrs, "num_hooks") != "3" || len(attrs) != 2 || unclassified["x.new_note"] != 1 {
		t.Fatalf("attrs %v, unclassified %v", attrs, unclassified)
	}
}
