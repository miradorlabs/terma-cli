package codex

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names a Codex thread by conversation.id, thread.id or thread_id; a numeric
// thread id is an OS thread, never a session.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{
		SessionKeys: []shape.SessionKey{
			{Attr: "conversation.id", Rank: 20},
			{Attr: "thread.id", Rank: 40, RejectNumeric: true},
			{Attr: "thread_id", Rank: 50, RejectNumeric: true},
		},
		StartEvents: []string{"codex.conversation_starts"},
	}
}

// CaptureRules are where Codex's telemetry carries content, blanked with Codex's own
// marker so the backend sees the shape it already parses.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		PromptFields:      []string{"prompt"},
		ToolContentFields: []string{"arguments", "output"},
		Marker:            "[REDACTED]",
		MarkerKeys:        []string{"conversation.id", "thread.id"},
		SafeKeys:          []string{"codex.request.reasoning_effort", "codex.turn.reasoning_effort", "auth.env_codex_api_key_enabled", "auth.env_codex_api_key_present", "auth.env_openai_api_key_present", "auth.env_refresh_token_url_override_present", "auth.header_attached", "auth.retry_after_unauthorized", "app_server.api_version", "app_server.client_name", "app_server.client_version", "app_server.connection_id", "codex.op", "conversation.id", "thread.id", "thread_id", "turn.id", "call_id", "event.kind", "provider_name", "slug", "reasoning_effort", "reasoning_summary", "model_reasoning_effort", "approval_policy", "sandbox_policy", "sandbox", "auth_mode", "originator", "app.version", "session_source", "mcp_server", "mcp_server_origin", "mcp_servers", "agent_name", "tmp_mem_enabled", "token_type", "tool_namespace", "command_category", "tool_result_seq", "output_truncated", "input_token_count", "output_token_count", "cached_token_count", "cache_write_token_count", "reasoning_token_count", "tool_token_count", "busy_ns", "idle_ns", "target", "hook_event_name", "turn_id", "os", "os_version", "env", "wire_api", "hook.command_outcome", "hook.display_order", "hook.event_name", "hook.execution_mode", "hook.handler_type", "hook.scope", "hook.source", "hook.timeout_sec", "unified_exec_process_id", "submission.id"},
		SafePrefixes:      []string{"codex.turn.token_usage.", "codex.usage."},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
