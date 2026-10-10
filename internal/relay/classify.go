package relay

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/semconv"
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

// classify mirrors withholdAttrs and withhold's resource and span-event passes.
func (ru *rules) classify(key string) FieldClass {
	if rest, ok := strings.CutPrefix(key, "event/"); ok {
		// An event's name may hold slashes (an agent may name its events by source file); a key does not.
		i := strings.LastIndex(rest, "/")
		event, k := rest[:max(i, 0)], rest[i+1:]
		if !contains(ru.toolContentEvents, event) {
			return ru.classify(k)
		}
		// A tool-content event goes whole when tool content is withheld; its prompt keys go
		// with prompts too, and the rest, unclassified included, is the tool's output.
		if ru.contentKey(k) && !contains(ru.toolContentFields, k) {
			return FieldPrompt
		}
		return FieldToolContent
	}
	if rk, ok := strings.CutPrefix(key, "resource/"); ok {
		switch {
		case contains(ru.resourcePromptFields, rk):
			return FieldPrompt
		case rk == semconv.TermaRepositoryRootKey, rk == semconv.TermaWorkingDirectoryKey:
			return FieldToolContent
		case ru.safeKey(rk):
			return FieldSafe
		}
		return FieldUnclassified
	}
	switch {
	case contains(ru.toolContentFields, key):
		return FieldToolContent
	case ru.contentKey(key):
		return FieldPrompt
	case ru.safeKey(key):
		return FieldSafe
	}
	return FieldUnclassified
}
