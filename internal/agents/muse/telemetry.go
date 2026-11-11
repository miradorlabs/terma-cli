package muse

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// Muse's own telemetry is OTLP, protobuf, to <telemetry.endpoint>/muse-code/telemetry/{logs,traces}
// (https://api.meta.ai by default): a log event and a span for each session, turn, model call,
// tool call and subagent, with ids, timings, token counts, the build and the workspace's
// repository and commit, and no prompt, reply or tool input or output. Every record names the
// session its hooks claim in session_root_id, a reminder agent's beside the main thread's too.

// museTelemetryPrefix is the path Muse puts after its telemetry endpoint, before the signal.
const museTelemetryPrefix = "/muse-code/telemetry/"

// metaTelemetry is where Muse sends its telemetry by default.
const metaTelemetry = "https://api.meta.ai"

// sessionRoot names the session a Muse record belongs to: the root its hooks claim.
var sessionRoot = shape.SessionKey{Attr: "session_root_id", Rank: 20, Claimed: true}

// Correlation places Muse's records in the session its hooks claimed.
func (Agent) Correlation() shape.Correlation {
	return shape.Correlation{SessionKeys: []shape.SessionKey{sessionRoot}}
}

// CaptureRules: Muse's telemetry carries no content, so every key it names that the relay does
// not already pass is safe: ids, the model and tool names, states from a fixed vocabulary, the
// build, and the workspace's repository and commit, which the relay stamps on every record too.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{SafeKeys: []string{
		// The resource.
		"app_build_sha", "app_version", "arch", "fb.logs.scuba.table", "fb.traces.scuba.table",
		"gen_ai_semconv_version", "platform", "surface", "workspace_repo",
		// Sessions, turns and subagents.
		"session_id", "session_root_id", "session_kind", "subagent_session_id", "trajectory_id",
		"trajectory_parent_id", "message_id", "event_name", "credential_backend",
		"keychain_fallback_reason", "security_mode", "input_modality", "exit_reason", "final_status",
		"reminder_outcome", "model_id", "tool_surface_version", "workspace_commit", "workspace_kind",
		"workspace_vcs",
		// Model and tool calls.
		"gen_ai_provider_name", "gen_ai_request_model", "gen_ai_response_model", "gen_ai_response_id",
		"gen_ai_response_finish_reasons", "gen_ai_operation_name", "gen_ai_conversation_id",
		"gen_ai_agent_name", "gen_ai_tool_name", "gen_ai_tool_call_id", "gen_ai_tool_type",
		"failure_kind", "backgrounded",
	}}
}

// Mirrors: pointed at the relay, Muse's telemetry still reaches Meta, as Muse sent it.
func (Agent) Mirrors() []shape.Mirror {
	return []shape.Mirror{{Prefix: museTelemetryPrefix, Upstream: metaTelemetry}}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
	_ shape.Mirrored   = Agent{}
)
