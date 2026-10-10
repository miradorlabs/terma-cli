package relay

import (
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// FieldClass is what the content policy does with an attribute key when a project withholds
// prompts and tool content: the field catalog's classification of every key an agent sends.
type FieldClass string

const (
	// FieldSafe leaves whatever the policy.
	FieldSafe FieldClass = "safe"
	// FieldPrompt is what was said: masked or dropped when prompts are withheld.
	FieldPrompt FieldClass = "prompt"
	// FieldToolContent is a tool's input or output, or a local path: dropped when tool
	// content is withheld.
	FieldToolContent FieldClass = "tool_content"
	// FieldUnclassified is a key no rule names: withheld, and counted, unless its value is a
	// number or a boolean, which cannot carry what was said.
	FieldUnclassified FieldClass = "unclassified"
)

// Classify says what the relay, composed from capturers as it runs, does with each key. A
// key prefixed "resource/" is a resource attribute's, as the relay counts those, and one
// prefixed "event/<name>/" is on a span event of that name.
func Classify(capturers []shape.Capturer, keys []string) map[string]FieldClass {
	ru := compose(nil, capturers)
	out := make(map[string]FieldClass, len(keys))
	for _, k := range keys {
		out[k] = ru.classify(k)
	}
	return out
}

// sample is the text value a key is classified with: what was said, as far as any rule knows.
const sample = "what was said"

// classify runs key, with a text value, through the content policy itself (withhold), where
// key says it sits, and reports what became of it: counted as unclassified is unclassified
// (the relay drops such a key under any policy that withholds anything), masked or dropped
// with only prompts withheld is prompt, dropped with only tool content withheld is tool
// content, and kept through both is safe.
func (ru *rules) classify(key string) FieldClass {
	at := func(prompts, toolContent bool) (kv *commonpb.KeyValue, unclassified bool) {
		p, find := placed(key)
		counted := map[string]int{}
		ru.withhold(p, prompts, toolContent, counted)
		return find(), len(counted) > 0
	}
	if _, unclassified := at(false, false); unclassified {
		return FieldUnclassified
	}
	if kv, _ := at(false, true); kv == nil || kv.GetValue().GetStringValue() != sample {
		return FieldPrompt
	}
	if kv, _ := at(true, false); kv == nil {
		return FieldToolContent
	}
	return FieldSafe
}

// placed builds a part carrying key with the sample value where key says it sits, and a
// function that finds it in the part once the policy has been applied, nil when it is gone.
func placed(key string) (*part, func() *commonpb.KeyValue) {
	attr := func(k string) []*commonpb.KeyValue {
		return []*commonpb.KeyValue{{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: sample}}}}
	}
	get := func(attrs []*commonpb.KeyValue, k string) *commonpb.KeyValue {
		for _, kv := range attrs {
			if kv.GetKey() == k {
				return kv
			}
		}
		return nil
	}
	if rest, ok := strings.CutPrefix(key, "event/"); ok {
		// An event's name may hold slashes (an agent may name its events by source file); a key does not.
		i := strings.LastIndex(rest, "/")
		event, k := rest[:max(i, 0)], rest[i+1:]
		sp := &tracepb.Span{Name: "span", Events: []*tracepb.Span_Event{{Name: event, Attributes: attr(k)}}}
		p := &part{signal: Traces, msg: &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}}}}
		return p, func() *commonpb.KeyValue {
			if len(sp.Events) == 0 {
				return nil
			}
			return get(sp.Events[0].Attributes, k)
		}
	}
	if k, ok := strings.CutPrefix(key, "resource/"); ok {
		res := &resourcepb.Resource{Attributes: attr(k)}
		p := &part{signal: Logs, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: res,
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}}}}}}
		return p, func() *commonpb.KeyValue { return get(res.Attributes, k) }
	}
	rec := &logspb.LogRecord{Attributes: attr(key)}
	p := &part{signal: Logs, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: &resourcepb.Resource{},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{rec}}}}}}}
	return p, func() *commonpb.KeyValue { return get(rec.Attributes, key) }
}
