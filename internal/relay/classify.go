package relay

import (
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

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
	// FieldUnclassified is a key no rule names: withheld, and counted, under any policy that
	// withholds content, but for the kinds of value FieldVerdict.Kept names.
	FieldUnclassified FieldClass = "unclassified"
)

// FieldSite is where an attribute sits, as the content policy tells them apart.
type FieldSite string

const (
	// SiteRecord is a log record's attribute, and as the policy treats them, a span's, a
	// span link's, a metric point's, an exemplar's and the instrumentation scope's.
	SiteRecord FieldSite = "record"
	// SiteResource is a resource attribute.
	SiteResource FieldSite = "resource"
	// SiteEvent is a span event's attribute: the policy treats some events whole.
	SiteEvent FieldSite = "event"
)

// FieldQuery is a key to classify, and where it sits; Event names the span event of one on
// SiteEvent.
type FieldQuery struct {
	Site  FieldSite `json:"site"`
	Event string    `json:"event,omitempty"`
	Key   string    `json:"key"`
}

// FieldVerdict is what the content policy does with one key: its class, and the kinds of
// value it keeps when a project withholds both prompts and tool content.
type FieldVerdict struct {
	Class FieldClass `json:"class"`
	Kept  []string   `json:"kept"`
}

// sample is the text value a key is classified with: what was said, as far as any rule knows.
const sample = "what was said"

// kinds are the kinds of value a verdict names, each with a value of that kind.
var kinds = []struct {
	name  string
	value *commonpb.AnyValue
}{
	{"text", &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: sample}}},
	{"number", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}},
	{"bool", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
	{"list", &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{}}}},
	{"map", &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{}}}},
	{"bytes", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte(sample)}}},
}

// Classify says what the relay, composed from capturers as it runs, does with each key, in
// the order asked.
func Classify(capturers []shape.Capturer, queries []FieldQuery) []FieldVerdict {
	ru := compose(nil, capturers)
	out := make([]FieldVerdict, len(queries))
	for i, q := range queries {
		out[i] = FieldVerdict{Class: ru.classify(q), Kept: []string{}}
		for _, k := range kinds {
			if kv, _ := ru.apply(q, k.value, false, false); kv != nil && proto.Equal(kv.GetValue(), k.value) {
				out[i].Kept = append(out[i].Kept, k.name)
			}
		}
	}
	return out
}

// classify runs q's key, with a text value, through the content policy itself (withhold),
// and reports what became of it: counted as unclassified is unclassified (the relay drops
// such a key under any policy that withholds anything), masked or dropped with only prompts
// withheld is prompt, dropped with only tool content withheld is tool content, and kept
// through both is safe.
func (ru *rules) classify(q FieldQuery) FieldClass {
	text := kinds[0].value
	if _, unclassified := ru.apply(q, text, false, false); unclassified {
		return FieldUnclassified
	}
	if kv, _ := ru.apply(q, text, false, true); kv == nil || !proto.Equal(kv.GetValue(), text) {
		return FieldPrompt
	}
	if kv, _ := ru.apply(q, text, true, false); kv == nil {
		return FieldToolContent
	}
	return FieldSafe
}

// apply puts q's key with value v where q says it sits, withholds what the policy collecting
// prompts and tool content as given withholds, and returns the key as it came out, nil when
// it is gone, and whether it was counted as unclassified.
func (ru *rules) apply(q FieldQuery, v *commonpb.AnyValue, prompts, toolContent bool) (*commonpb.KeyValue, bool) {
	attrs := []*commonpb.KeyValue{{Key: q.Key, Value: proto.Clone(v).(*commonpb.AnyValue)}}
	var p *part
	var after func() []*commonpb.KeyValue
	switch q.Site {
	case SiteEvent:
		sp := &tracepb.Span{Name: "span", Events: []*tracepb.Span_Event{{Name: q.Event, Attributes: attrs}}}
		p = &part{signal: Traces, msg: &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{sp}}}}}}}
		after = func() []*commonpb.KeyValue {
			if len(sp.Events) == 0 {
				return nil
			}
			return sp.Events[0].Attributes
		}
	case SiteResource:
		res := &resourcepb.Resource{Attributes: attrs}
		p = &part{signal: Logs, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: res,
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{}}}}}}}}
		after = func() []*commonpb.KeyValue { return res.Attributes }
	default:
		rec := &logspb.LogRecord{Attributes: attrs}
		p = &part{signal: Logs, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: &resourcepb.Resource{},
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{rec}}}}}}}
		after = func() []*commonpb.KeyValue { return rec.Attributes }
	}
	counted := map[string]int{}
	ru.withhold(p, prompts, toolContent, counted)
	for _, kv := range after() {
		if kv.GetKey() == q.Key {
			return kv, len(counted) > 0
		}
	}
	return nil, len(counted) > 0
}
