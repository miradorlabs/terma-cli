package omp

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names omp's session: gen_ai.conversation.id, on its native invoke_agent, chat
// and execute_tool spans; a subagent's spans carry its own. Its logs and metrics name no
// session, and go by the process that sent them.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.GenAIConversationID}}
}

// CaptureRules are where omp's native telemetry carries content beyond the GenAI attributes
// every agent shares, and the keys of its own that carry none.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		PromptDropFields: []string{"omp.gen_ai.request.messages", "omp.gen_ai.response.text"},
		// The request restates every tool result in the conversation, and the response's
		// tool calls are their input.
		ToolContentFields: []string{"omp.gen_ai.request.messages", "omp.gen_ai.response.tool_calls"},
		// The prompt log of the span-exporting extension an earlier terma wrote, until setup
		// replaces it.
		PromptBodyEvents: []string{"omp.user_prompt"},
		// Strings and lists only: a number or a flag passes whatever its key (cost, token
		// counts, latencies). Each is one omp 19.0.1 sends, read from its own export.
		SafeKeys: []string{
			// A tool call's outcome (ok, error, skipped, blocked, timeout, aborted), a chat's
			// stop reason, and the reasoning effort it asked for.
			"omp.gen_ai.tool.status", "omp.tool.status", "gen_ai.response.finish_reason", "omp.gen_ai.request.reasoning.effort",
			// The provider gateway by name, its base URL (as server.address names a server)
			// and its request id.
			"omp.gen_ai.gateway.name", "omp.gen_ai.gateway.endpoint", "omp.gen_ai.gateway.call_id",
			// A subagent's id and name (its description is the agent's prose), and where a
			// handoff went.
			"gen_ai.agent.id", "omp.gen_ai.agent.id", "omp.gen_ai.agent.name",
			"omp.gen_ai.handoff.to_agent.id", "omp.gen_ai.handoff.to_agent.name",
			// The tools, models and providers a run had and used, by name.
			"omp.gen_ai.request.available_tools", "omp.gen_ai.agent.tools.available",
			"omp.gen_ai.agent.tools.invoked", "omp.gen_ai.agent.tools.unused",
			"omp.agent.coverage.tools_available", "omp.agent.coverage.tools_invoked", "omp.agent.coverage.tools_unused",
			"omp.agent.coverage.models_used", "omp.agent.coverage.providers_used", "omp.agent.cost.unavailable_reasons",
		},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
