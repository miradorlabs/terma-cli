package hookrun

// maxAttrLen bounds a string a harness supplied before it travels as an attribute. An
// identifier or a model name is a few dozen bytes; anything past this is not one.
const maxAttrLen = 1024

// boundedAttr sets attrs[key] to v when v is present and within maxAttrLen. A value past
// the bound is left out rather than cut: a truncated identifier joins to nothing, and
// missing stays missing everywhere else in these events.
func boundedAttr(attrs map[string]any, key, v string) {
	if v != "" && len(v) <= maxAttrLen {
		attrs[key] = v
	}
}

// evidenceAttrs opens a record a hooks-only harness reports: which agent, which of its
// hooks, and that the only order terma can vouch for is the order it received them in.
func evidenceAttrs(tool, source, hook string) map[string]any {
	return map[string]any{
		attrTool: tool, attrSchemaVersion: 1, attrEvidenceSource: source,
		attrHookEvent: hook, "ordering": "local_receipt",
	}
}
