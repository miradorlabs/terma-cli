package grok

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// logScope is the instrumentation scope of every record grok exports.
const logScope = "ai.xai.grok_code"

// Correlation names grok's session: session.id, on every session record of its logs. A
// subagent's records carry its own, in its parent's process. The key is Claimed on grok's
// scope: a hook claims every session grok runs (a subagent's at SubagentStart), so global
// mode's catch-all waits for the claim, and with it the parent the relay stamps, rather than
// sending a session's first batch to the default project. Records with no session (auth,
// MCP connections, a subagent's launch) go by process.
func (Agent) Correlation() shape.Correlation {
	key := shape.SessionID
	key.Claimed, key.Scope = true, logScope
	return shape.Correlation{SessionKeys: []shape.SessionKey{key}}
}

// CaptureRules are where grok's export carries content beyond what Claude Code's and the
// generic rules name (prompt and response, tool_parameters, tool_input, full_command,
// file_path), and the string keys of its own that carry none. user.email is left
// unclassified on purpose: it leaves only when the policy collects everything.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		// A tool's output, and an error that may quote it.
		ToolContentFields: []string{"tool_output", "error_message"},
		// grok writes the home directory as ~ in every string it exports, a file_path included.
		HomeRelativeScopes: []string{logScope},
		// Strings only: a number or a flag passes whatever its key (tokens, cost, counts,
		// durations, turn_number). Each is one grok 1.0.92 sends, read from its own export.
		SafeKeys: []string{
			// Models by the names the developer's config gives them.
			"from_model", "to_model",
			// grok's own vocabularies: the permission system's kind of access, why a turn
			// failed or was cancelled, the screen a prompt came from, the client, an MCP
			// connection's transport, a sign-in's method.
			"access_kind", "error_category", "cancellation_category", "screen_mode",
			"client_identifier", "transport_type", "auth_method",
			// A file tool's extension, never its path.
			"file_extension",
			// MCP servers and tools by name, as tool_name names a tool.
			"mcp_tool.name", "mcp_server.name",
			// The xAI team a sign-in belongs to.
			"team.id",
			// The resource: grok's build, process and schema.
			"client.version", "service.instance.id", "app.entrypoint", "grok_code.schema.version", "grok_code.release_line",
		},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
