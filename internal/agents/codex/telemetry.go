package codex

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Correlation names a Codex thread: conversation.id on logs, thread.id on the turn span,
// thread_id on session_loop. A numeric thread id is an OS thread, never a session.
// app-server exports conversation_starts at thread/start, long before the first hook.
// The turn span is exported when the turn ends, and its children carry only the trace:
// the mid-turn logs that carry both teach the relay the trace's session. Codex's
// metrics and process-level spans name no session; the relay places them by process
// exit, and app-server, shared by every thread, rarely exits.
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
		SafeKeys:          []string{"codex.request.reasoning_effort", "codex.turn.reasoning_effort", "auth.env_codex_api_key_enabled", "auth.env_codex_api_key_present", "auth.env_openai_api_key_present", "auth.env_refresh_token_url_override_present", "auth.header_attached", "auth.retry_after_unauthorized", "app_server.api_version", "app_server.client_name", "app_server.client_version", "app_server.connection_id", "codex.op"},
		SafePrefixes:      []string{"codex.turn.token_usage.", "codex.usage."},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
