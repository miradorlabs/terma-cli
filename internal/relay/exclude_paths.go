package relay

import (
	"encoding/base64"
	"strconv"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// pathExcluded reports whether any attribute names an excluded file, before content
// filtering removes them; reflection reaches attributes wherever OTLP keeps them.
func pathExcluded(msg proto.Message, excludes func(any) bool) bool {
	if excludes == nil {
		return false
	}
	return excludedIn(msg.ProtoReflect(), excludes)
}

func excludedIn(m protoreflect.Message, excludes func(any) bool) bool {
	// protojson leaves an empty key out, and an attribute without one names nothing.
	if kv, ok := m.Interface().(*commonpb.KeyValue); ok && kv.GetKey() != "" {
		if excludes(map[string]any{kv.GetKey(): anyValueJSON(kv.GetValue())}) {
			return true
		}
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
			return true
		}
		switch {
		case fd.IsMap():
			v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
				found = excludedIn(mv.Message(), excludes)
				return !found
			})
		case fd.IsList():
			for i, list := 0, v.List(); i < list.Len() && !found; i++ {
				found = excludedIn(list.Get(i).Message(), excludes)
			}
		default:
			found = excludedIn(v.Message(), excludes)
		}
		return !found
	})
	return found
}

// anyValueJSON is v in protojson's shape, the one Policy.Excludes reads.
func anyValueJSON(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return map[string]any{"stringValue": x.StringValue}
	case *commonpb.AnyValue_BoolValue:
		return map[string]any{"boolValue": x.BoolValue}
	case *commonpb.AnyValue_IntValue:
		return map[string]any{"intValue": strconv.FormatInt(x.IntValue, 10)}
	case *commonpb.AnyValue_DoubleValue:
		return map[string]any{"doubleValue": x.DoubleValue}
	case *commonpb.AnyValue_BytesValue:
		return map[string]any{"bytesValue": base64.StdEncoding.EncodeToString(x.BytesValue)}
	case *commonpb.AnyValue_ArrayValue:
		values := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			values = append(values, anyValueJSON(e))
		}
		return map[string]any{"arrayValue": map[string]any{"values": values}}
	case *commonpb.AnyValue_KvlistValue:
		values := make([]any, 0, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			entry := map[string]any{"value": anyValueJSON(kv.GetValue())}
			if kv.GetKey() != "" {
				entry["key"] = kv.GetKey()
			}
			values = append(values, entry)
		}
		return map[string]any{"kvlistValue": map[string]any{"values": values}}
	}
	return map[string]any{}
}
