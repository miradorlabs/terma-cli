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
		PromptDropFields:     []string{"request_text", "response_text"},
		ResourcePromptFields: []string{"process.command_args"},
		ToolContentFields:    []string{"function_args", "hook_input", "hook_output", "stdout", "stderr"},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
