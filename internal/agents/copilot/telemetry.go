package copilot

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names the session: gen_ai.conversation.id on every span of the Copilot
// runtime, which is the session_id its hooks claim. Its metrics name no session, and go by
// the process that sent them.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.GenAIConversationID}}
}

// CaptureRules are where the Copilot runtime's telemetry carries content beyond the GenAI
// attributes every agent shares: it restates a shell tool's command and a file tool's path.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		ToolContentFields: []string{"github.copilot.tool.parameters.command", "github.copilot.tool.parameters.file_path"},
		// Strings only: a number or a flag passes whatever its key (a request's AI credits,
		// its billing multiplier), and a key no rule names is withheld with the content.
		// Each is one CLI 1.0.96 sends, read from its own export, but for the last three.
		SafeKeys: []string{
			// The ids of a turn and of its requests, who started a request (user or agent),
			// the kind of agent, a tool's kind, a command's exit status, the account's
			// pseudonymous id and the exporter's instance.
			"github.copilot.interaction_id", "github.copilot.turn_id", "github.copilot.turn_count", "github.copilot.initiator",
			"github.copilot.service_request_id", "github.copilot.agent.type", "gen_ai.request.previous_response.id",
			"gen_ai.tool.type", "gen_ai.token.modality", "process.exit.code", "enduser.pseudo.id", "service.instance.id",
			// Its span events: a hook's type, id, decision and the tools it ran for, where a
			// user message came from, an MCP server by name and state, and the shell sandbox's
			// posture, each a fixed vocabulary.
			"github.copilot.hook.type", "github.copilot.hook.invocation_id", "github.copilot.hook.decision", "github.copilot.hook.tool_names",
			"github.copilot.user.message.source", "github.copilot.user.message.interaction_id", "github.copilot.user.message.turn_id",
			"github.copilot.context.mcp_server_names", "github.copilot.mcp.server.name", "github.copilot.mcp.server.name_hash",
			"github.copilot.mcp.server.state", "github.copilot.mcp.server.source",
			"github.copilot.sandbox.backend", "github.copilot.sandbox.control", "github.copilot.sandbox.decision_kind",
			"github.copilot.sandbox.enforcement_point", "github.copilot.sandbox.outcome", "github.copilot.sandbox.platform",
			// How a call failed, as a code and a class (a tool denied its permission sends
			// "denied" and "ToolError"), and what a permission was asked for and came to. The
			// failure's own words, exception.message, are no rule's and stay withheld.
			"error.type", "exception.type", "github.copilot.error_code",
			"github.copilot.permission.kind", "github.copilot.permission.result",
			// Named by the runtime, on no captured span: an MCP tool's server and tool, which
			// gen_ai.tool.name and the server's lifecycle events say already.
			"github.copilot.tool.parameters.mcp_server_name", "github.copilot.tool.parameters.mcp_tool_name",
			// The name VS Code's agent host gives the service it runs the runtime as, read
			// from VS Code's source: what tells its sessions from the CLI's.
			"service.namespace",
		},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
