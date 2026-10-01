package hookrun

const maxAttrLen = 1024

// BoundedAttr sets attrs[key] to v when present and within maxAttrLen; a longer value is
// left out, not cut, since a truncated identifier joins to nothing.
func BoundedAttr(attrs map[string]any, key, v string) {
	if v != "" && len(v) <= maxAttrLen {
		attrs[key] = v
	}
}

// EvidenceAttrs opens a hooks-only agent's record, ordered only by local receipt.
func EvidenceAttrs(tool, source, hook string) map[string]any {
	return map[string]any{
		AttrTool: tool, AttrSchemaVersion: 1, AttrEvidenceSource: source,
		AttrHookEvent: hook, "ordering": "local_receipt",
	}
}
