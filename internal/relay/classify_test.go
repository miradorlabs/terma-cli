package relay

import (
	"maps"
	"slices"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// Classify says what withholdAttrs does, key by key, for every key a rule names and for
// keys none does: the field catalog is only worth what the relay actually does.
func TestClassifyAgreesWithTheContentPolicy(t *testing.T) {
	t.Parallel()
	ru := compose(testCorrelators, testCapturers)
	keys := map[string]bool{"from_mode": true, "a.key.no.rule.names": true, "product_sku": true}
	for _, set := range [][]string{ru.promptFields, ru.promptDropFields, ru.resourcePromptFields, ru.toolContentFields, slices.Collect(maps.Keys(ru.safeKeys))} {
		for _, k := range set {
			keys[k] = true
		}
	}
	marker := defaultMarker
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		class := ru.classify(key)
		unclassified := map[string]int{}
		out, _ := ru.withholdAttrs([]*commonpb.KeyValue{{Key: key, Value: strValue("what was said")}}, false, false, false, unclassified)
		var did string
		switch {
		case unclassified[key] > 0:
			did = "counted unclassified"
		case len(out) == 0:
			did = "dropped"
		case out[0].GetValue().GetStringValue() == marker:
			did = "masked"
		default:
			did = "kept"
		}
		want := map[FieldClass][]string{
			FieldSafe:         {"kept"},
			FieldPrompt:       {"dropped", "masked"},
			FieldToolContent:  {"dropped"},
			FieldUnclassified: {"counted unclassified"},
		}[class]
		if !slices.Contains(want, did) {
			t.Errorf("%s: classified %s, but withheld content %s it", key, class, did)
		}
	}
}

// A resource key is classified as the relay's resource pass treats it, and a span event's
// key as its event is treated.
func TestClassifyResourceKeys(t *testing.T) {
	t.Parallel()
	got := Classify(testCapturers, []string{"resource/service.name", "resource/process.command_args", "resource/terma.repository.root", "resource/a.custom.key",
		"event/tool.output/content", "event/tool.output/prompt", "event/tool.output/tool_name", "event/exception/a.custom.key",
		"event/event otel/src/tool_result.rs/auth_mode"})
	want := map[string]FieldClass{
		"resource/service.name":          FieldSafe,
		"resource/process.command_args":  FieldPrompt,
		"resource/terma.repository.root": FieldToolContent,
		"resource/a.custom.key":          FieldUnclassified,
		// Claude's tool.output is a tool-content event: it goes whole, its prompt keys with prompts.
		"event/tool.output/content":    FieldToolContent,
		"event/tool.output/prompt":     FieldPrompt,
		"event/tool.output/tool_name":  FieldToolContent,
		"event/exception/a.custom.key": FieldUnclassified,
		// Codex names its events by source file: the key is what follows the last slash.
		"event/event otel/src/tool_result.rs/auth_mode": FieldSafe,
	}
	if !maps.Equal(got, want) {
		t.Errorf("Classify = %v, want %v", got, want)
	}
}

// A process's arguments are what it was asked to do: withheld with prompts on a record as on
// its resource. A record carrying them used to send them with content withheld.
func TestProcessArgumentsOnARecordAreWithheld(t *testing.T) {
	t.Parallel()
	ru := compose(testCorrelators, testCapturers)
	for _, key := range []string{"process.command_args", "process.command_line"} {
		out, _ := ru.withholdAttrs([]*commonpb.KeyValue{{Key: key, Value: strValue("terma --token secret")}, {Key: "model", Value: strValue("m")}}, false, true, false, map[string]int{})
		if len(out) != 1 || out[0].GetKey() != "model" {
			t.Errorf("%s on a record with prompts withheld: %v", key, out)
		}
		out, _ = ru.withholdAttrs([]*commonpb.KeyValue{{Key: key, Value: strValue("terma --token secret")}}, true, true, false, map[string]int{})
		if len(out) != 1 {
			t.Errorf("%s on a record with prompts collected: %v", key, out)
		}
	}
}
