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
		class := ru.classify(FieldQuery{Site: SiteRecord, Key: key})
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
// key as its event is treated; each verdict names the kinds of value the key keeps with all
// content withheld, which for an unclassified key differs by where it sits.
func TestClassifyWhereKeysSit(t *testing.T) {
	t.Parallel()
	all := []string{"text", "number", "bool", "list", "map", "bytes"}
	for _, c := range []struct {
		q     FieldQuery
		class FieldClass
		kept  []string
	}{
		{FieldQuery{Site: SiteRecord, Key: "from_mode"}, FieldSafe, all},
		{FieldQuery{Site: SiteRecord, Key: "prompt"}, FieldPrompt, []string{}},
		// A number or a flag cannot carry what was said: an unclassified one on a record passes.
		{FieldQuery{Site: SiteRecord, Key: "a.key.no.rule.names"}, FieldUnclassified, []string{"number", "bool"}},
		{FieldQuery{Site: SiteResource, Key: "service.name"}, FieldSafe, all},
		{FieldQuery{Site: SiteResource, Key: "process.command_args"}, FieldPrompt, []string{}},
		{FieldQuery{Site: SiteResource, Key: "terma.repository.root"}, FieldToolContent, []string{}},
		// On a resource, an unclassified key goes whatever its value.
		{FieldQuery{Site: SiteResource, Key: "process.parent_pid"}, FieldUnclassified, []string{}},
		// Claude's tool.output is a tool-content event: it goes whole, its prompt keys with prompts.
		{FieldQuery{Site: SiteEvent, Event: "tool.output", Key: "content"}, FieldToolContent, []string{}},
		{FieldQuery{Site: SiteEvent, Event: "tool.output", Key: "prompt"}, FieldPrompt, []string{}},
		{FieldQuery{Site: SiteEvent, Event: "tool.output", Key: "tool_name"}, FieldToolContent, []string{}},
		{FieldQuery{Site: SiteEvent, Event: "exception", Key: "a.custom.key"}, FieldUnclassified, []string{"number", "bool"}},
		// Codex names its events by source file; a slash in either name is no separator.
		{FieldQuery{Site: SiteEvent, Event: "event otel/src/tool_result.rs", Key: "auth_mode"}, FieldSafe, all},
		{FieldQuery{Site: SiteEvent, Event: "exception", Key: "a/b"}, FieldUnclassified, []string{"number", "bool"}},
	} {
		got := Classify(testCapturers, []FieldQuery{c.q})
		if len(got) != 1 || got[0].Class != c.class || !slices.Equal(got[0].Kept, c.kept) {
			t.Errorf("Classify(%+v) = %+v, want %s keeping %v", c.q, got, c.class, c.kept)
		}
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
