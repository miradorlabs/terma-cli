package gemini

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names the session: session.id on logs and metrics, gen_ai.conversation.id on spans.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.SessionID, shape.GenAIConversationID}}
}

// CaptureRules are where Gemini CLI's telemetry carries content; the resource's command
// line carries a -p prompt whatever logPrompts says.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		PromptDropFields:     []string{"request_text", "response_text", "routing.reasoning"},
		ResourcePromptFields: []string{"process.command_args"},
		ToolContentFields:    []string{"function_args", "hook_input", "hook_output", "stdout", "stderr"},
		SafeKeys:             []string{"embedding_model", "core_tools_enabled", "approval_mode", "mcp_tools", "mcp_tools_count", "mcp_server_name", "output_format", "extensions", "extension_ids", "extension_name", "extension_id", "auth_type", "function_name", "tool_type", "operation", "mimetype", "extension", "programming_language", "finish_reasons", "decision_model", "decision_source", "os_arch", "installation.id", "routing.decision_model", "routing.decision_source", "routing.approval_mode"},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
