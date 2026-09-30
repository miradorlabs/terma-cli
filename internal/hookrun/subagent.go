package hookrun

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// Subagents come in two shapes and the events keep them apart.
//
// Claude Code and Codex run a subagent inside the parent session: every hook payload
// keeps the parent's session_id and adds agent_id / agent_type, so the subagent is a
// facet of one session. terma.subagent.start / terma.subagent.end bracket it and
// terma.files.touched carries the same agent_id, all under the parent's session id.
//
// Cursor, OpenCode and a Codex thread spawn give the child its own conversation,
// session or rollout. Those are sessions of their own, and their terma.session.start
// names the parent in parent_session_id (OpenCode's Session.parentID, the Codex
// rollout's session_meta.source.subagent.thread_spawn). Cursor reports a subagent's end
// under the parent conversation (subagentStop), which is where its outcome and the
// files it changed are recorded.
//
// None of this is token usage: subagent spend rides the native OTel export (Claude's
// query_source and agent.name), never these events.

// AgentAttrs stamps the agent facet on an event when the hook payload names an agent.
// agent_id is the discriminator: Claude Code sends agent_type on every hook of a
// `claude --agent <name>` session, subagent or not, and only agent_id says the hook
// fired inside a subagent — so a type without an id stamps nothing. The id is an
// identifier the harness mints and is held to a session id's charset; the type is a
// name a developer chose for the agent and is only kept single-line and short.
func AgentAttrs(attrs map[string]any, id, kind string) map[string]any {
	if !session.ValidID(id) {
		return attrs
	}
	attrs[attrAgentID] = id
	if ShortLabel(kind) {
		attrs[attrAgentType] = kind
	}
	return attrs
}

// ShortLabel reports whether v is fit to travel as a label: present, at most 128 bytes,
// on one line.
func ShortLabel(v string) bool {
	return v != "" && len(v) <= 128 && !strings.ContainsAny(v, "\r\n")
}
