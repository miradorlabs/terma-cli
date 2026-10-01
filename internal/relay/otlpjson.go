package relay

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
)

// OTLP/JSON sends trace and span ids as hex, which protojson silently reads as base64,
// breaking every trace join; hexIDsToBase64 rewrites them first.
var otlpIDFields = map[string]bool{"traceId": true, "spanId": true, "parentSpanId": true}

func hexIDsToBase64(body []byte) ([]byte, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if s, ok := child.(string); ok && otlpIDFields[k] {
					if raw, err := hex.DecodeString(s); err == nil && (len(raw) == 16 || len(raw) == 8) {
						t[k] = base64.StdEncoding.EncodeToString(raw)
					}
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(doc)
	return json.Marshal(doc)
}
