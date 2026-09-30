package relay

import (
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/config"
	"google.golang.org/protobuf/encoding/protojson"
)

// pathExcluded examines OTLP key/value attributes before content filtering removes
// them. A part belongs to one session; a named excluded file withholds that part.
func pathExcluded(p *part, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	b, err := protojson.Marshal(p.msg)
	if err != nil {
		return true
	}
	var data any
	if json.Unmarshal(b, &data) != nil {
		return true
	}
	pol := config.Policy{ExcludePaths: patterns}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			if key, ok := x["key"].(string); ok {
				value, _ := x["value"].(map[string]any)
				if pol.HasExcludedPath(map[string]any{key: value}, "") {
					return true
				}
			}
			for _, val := range x {
				if walk(val) {
					return true
				}
			}
		case []any:
			for _, val := range x {
				if walk(val) {
					return true
				}
			}
		}
		return false
	}
	return walk(data)
}
