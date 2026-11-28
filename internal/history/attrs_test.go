package history

import (
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func logRecordAttrString(lr *logspb.LogRecord, key string) string {
	for _, kv := range lr.GetAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func spanAttrString(sp *tracepb.Span, key string) string {
	for _, kv := range sp.GetAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}
