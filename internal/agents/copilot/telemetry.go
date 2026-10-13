package copilot

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names the session: gen_ai.conversation.id on the spans of the Copilot
// runtime and of VS Code's own agent, session.id on VS Code's session-start record. Both
// are the session_id the hooks claim.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.SessionID, shape.GenAIConversationID}}
}

// CaptureRules are where Copilot's telemetry carries content beyond the GenAI attributes
// every agent shares: the Copilot runtime restates a shell tool's command, and VS Code's
// own agent the request, its reply and reasoning, and its hooks' input and output.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		PromptDropFields: []string{"copilot_chat.user_request", "copilot_chat.markdown_content", "copilot_chat.reasoning_content"},
		ToolContentFields: []string{"github.copilot.tool.parameters.command",
			"copilot_chat.hook_command", "copilot_chat.hook_input", "copilot_chat.hook_output"},
		// Strings only: a number or a flag passes whatever its key (a request's AI credits,
		// its billing multiplier), and a key no rule names is withheld with the content.
		SafeKeys: []string{
			// The Copilot runtime (CLI 1.0.96): the ids of a turn and of its requests, who
			// started a request (user or agent), the kind of agent, a tool's kind, a
			// command's exit status, the account's pseudonymous id and the exporter's instance.
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
			// VS Code's own agent (Copilot Chat 0.69): the chat session and the one that
			// spawned it, the request's id at the service and its shape (the API, a message
			// count, the stream flag), a hook's type and outcome, and an error's class.
			"copilot_chat.session_id", "copilot_chat.chat_session_id", "copilot_chat.parent_chat_session_id",
			"copilot_chat.server_request_id", "copilot_chat.request.options", "copilot_chat.request.shape",
			"copilot_chat.hook_type", "copilot_chat.hook_result_kind", "error.type",
		},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
