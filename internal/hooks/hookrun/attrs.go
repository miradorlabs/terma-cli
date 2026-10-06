package hookrun

import (
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

const maxAttrLen = 1024

// BoundedAttr sets attrs[key] to v when present and within maxAttrLen; a longer value is
// left out, not cut, since a truncated identifier joins to nothing.
func BoundedAttr(attrs map[string]any, key, v string) {
	if v != "" && len(v) <= maxAttrLen {
		attrs[key] = v
	}
}

// EvidenceAttrs opens a hooks-only agent's record.
func EvidenceAttrs(tool, source, hook string) map[string]any {
	return map[string]any{
		semconv.GenAIMainAgentNameKey: tool, semconv.TermaEvidenceSourceKey: source, semconv.TermaHookEventKey: hook,
	}
}
