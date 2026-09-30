package claude

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names Claude Code's session on every signal.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.SessionID}}
}

// CaptureRules are where Claude Code's telemetry carries content. Tool content also rides
// the claude_code.tool span's tool.output event, which the golden attribute lists miss.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		PromptFields:      []string{"prompt", "response", "user_prompt"},
		ToolContentFields: []string{"tool_parameters", "tool_input", "full_command", "bash_command"},
		ToolContentEvents: []string{"tool.output", "tool.input"},
		BodyPrefixes:      []string{"claude_code."},
		SafeKeys:          []string{"plugin.name", "plugin.scope", "plugin_id_hash", "marketplace.name", "managed_settings.trigger", "managed_settings.sources", "managed_settings.source_behavior", "managed_settings.helper.state", "managed_settings.helper.applied"},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
