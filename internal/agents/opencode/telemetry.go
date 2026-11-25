package opencode

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// scope is the instrumentation scope of everything terma's plugin exports.
const scope = "terma-opencode"

// Correlation names the plugin's session: session.id, on every span and record. The key is
// Claimed on the plugin's scope: the plugin claims each session through its first hook
// before it exports anything of it, a subagent's too, so global mode's catch-all waits for
// the claim rather than sending a session's first batch to the default project.
func (Agent) Correlation() shape.Correlation {
	key := shape.SessionID
	key.Claimed, key.Scope = true, scope
	return shape.Correlation{SessionKeys: []shape.SessionKey{key}}
}

// CaptureRules are where the plugin's export carries content beyond the generic rules (the
// reply in gen_ai.completion, a tool's arguments and result), and the string keys of its own
// that carry none.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		// The prompt and the session's title, which restates it. opencode.session.created is
		// what a plugin written by an earlier terma still sends, titled.
		PromptBodyEvents: []string{"opencode.user_prompt", "opencode.session.title", "opencode.session.created"},
		// A provider's error can quote a prompt or a tool's arguments, so it leaves only when
		// both are collected, as the relay clears a span's status message, which the plugin
		// leaves empty.
		PromptDropFields: []string{"opencode.error.message"},
		// The files a file tool read or changed (a list of absolute paths) and the tool's own
		// one-line title, which names a file or a command. opencode.tool.file_path and
		// opencode.session.directory are local paths an earlier terma's plugin still sends.
		ToolContentFields: []string{"opencode.error.message", "opencode.tool.files", "opencode.tool.title",
			"opencode.tool.file_path", "opencode.session.directory"},
		// Strings only: a number passes whatever its key (tokens, cost, the reply's and the
		// prompt's lengths, a tool's output size and exit code).
		SafeKeys: []string{
			// The build, and the agent (build, plan, a subagent's) that made a call.
			"opencode.version", "opencode.agent",
			// Ids: a message, the prompt that opened its turn, the session that opened a
			// subagent's, a model call, and the session a task tool's subagent ran in.
			"opencode.message.id", "opencode.parent_message.id", "opencode.parent_session.id",
			"opencode.call.id", "opencode.tool.subagent_session.id",
			// The plugin's own vocabularies: what an auxiliary call was for (title,
			// compaction), how a tool call ended (completed, error, denied), what denied it
			// (rule, ask), and why a session was compacted (manual, auto).
			"opencode.call.kind", "opencode.tool.status", "opencode.tool.denial", "opencode.compaction.reason",
			// MCP servers and tools by name, as gen_ai.tool.name names a tool.
			"opencode.mcp.server", "opencode.mcp.tool",
			// What an earlier terma's plugin still sends: OpenCode's project hash.
			"opencode.project.id",
		},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
