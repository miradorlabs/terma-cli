package hookrun

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// A subagent either runs inside its parent's session, stamped with terma.agent.id, or is a
// session of its own naming terma.session.parent.id; neither carries token usage.

// AgentAttrs stamps the agent facet when the payload names an agent id; a type without an
// id stamps nothing, since an agent may send its type on every hook.
func AgentAttrs(attrs map[string]any, id, kind string) map[string]any {
	if !session.ValidID(id) {
		return attrs
	}
	attrs[semconv.TermaAgentIDKey] = id
	if ShortLabel(kind) {
		attrs[semconv.GenAIAgentNameKey] = kind
	}
	return attrs
}

// ShortLabel reports whether v is present, at most 128 bytes, and on one line.
func ShortLabel(v string) bool {
	return v != "" && len(v) <= 128 && !strings.ContainsAny(v, "\r\n")
}
