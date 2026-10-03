package claude

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names Claude Code's session on every signal.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{shape.SessionID}}
}

// CaptureRules are where Claude Code's telemetry carries content, the tool.output span event included.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		PromptFields:      []string{"prompt", "prompt_text", "response", "user_prompt"},
		ToolContentFields: []string{"tool_parameters", "tool_input", "full_command", "bash_command"},
		ToolContentEvents: []string{"tool.output", "tool.input"},
		BodyPrefixes:      []string{"claude_code."},
		SafeKeys:          []string{"plugin.name", "plugin.scope", "plugin_id_hash", "marketplace.name", "managed_settings.trigger", "managed_settings.sources", "managed_settings.source_behavior", "managed_settings.helper.state", "managed_settings.helper.applied", "prompt.id", "request_id", "message.uuid", "user.id", "event.sequence", "interaction.sequence", "span.type", "speed", "stop_reason", "start_type", "query_source", "query_source_safe", "parent.source", "tool_source", "queued_sends", "llm_request.context", "type", "tool_name_safe", "bash_argv0", "bash_command_class", "tool_input_size_bytes", "tool_result_size_bytes", "cache_creation_tokens", "cost_usd", "cost_usd_micros", "user_prompt_length", "first_content_ms", "interaction.duration_ms", "prompt_id", "enabled_via", "hook_matcher"},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
