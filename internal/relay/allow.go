package relay

import "strings"

// With a project's content withheld, a record leaves with the attributes known to say
// nothing of what was said, and no others. Removing the content fields the agents
// declare is not enough on its own: a release that adds one (a command line that
// carries a prompt, say) would leak until someone noticed. So every attribute key is
// classified. A content key keeps the treatment content.go gives it (a marker, or
// dropped); a safe key passes; any other key is dropped, and counted by name (Stats:
// unclassified.<key>) so that the live suite fails on it and a person decides which it
// is. A new field is then a visible loss, never a leak.
//
// The safe keys are these, which no one agent owns, and the ones each agent declares
// for its own telemetry (shape.CaptureRules.SafeKeys, SafePrefixes). The composed set is
// pinned in this package's tests: widening what leaves a machine is a reviewed edit
// here, whoever declares it. A key that is content for any agent is content.

// genericSafeKeys are attribute keys that never carry what was said, for any agent.
var genericSafeKeys = setOf(
	// Identity and correlation.
	"session.id", "conversation.id", "thread.id", "thread_id", "gen_ai.conversation.id", "prompt.id", "request_id",
	"message.uuid", "turn.id", "call_id", "tool_use_id", "gen_ai.tool.call.id", "gen_ai.response.id", "user.id",
	"event.name", "event.kind", "event.sequence", "event.timestamp", "interaction.sequence", "span.type",
	"mirador.project.id", "terma.relay.attribution", "terma.relay.session.id",
	// Models, providers and settings.
	"model", "gen_ai.request.model", "gen_ai.response.model", "gen_ai.system", "gen_ai.provider.name",
	"gen_ai.operation.name", "gen_ai.response.finish_reasons", "provider_name", "slug", "speed", "stop_reason",
	"reasoning_effort", "reasoning_summary", "model_reasoning_effort",
	"approval_policy", "sandbox_policy", "sandbox", "auth_mode", "originator",
	"app.version", "terminal.type", "session_source", "source", "start_type", "query_source", "query_source_safe",
	"parent.source", "tool_source", "mcp_server", "mcp_server_origin", "mcp_servers", "agent_name", "tmp_mem_enabled",
	"endpoint", "http.response.status_code", "status", "success", "decision", "attempt", "queued_sends",
	"llm_request.context", "type", "token_type",
	// Tools, by name and shape — never their input or output.
	"tool_name", "tool_name_safe", "tool", "tool_namespace", "gen_ai.tool.name", "bash_argv0", "bash_command_class",
	"command_category", "tool_input_size_bytes", "tool_result_size_bytes", "tool_result_seq", "output_truncated",
	// Counts, sizes, costs and timings.
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens", "cost_usd", "cost_usd_micros",
	"input_token_count", "output_token_count", "cached_token_count", "cache_write_token_count", "reasoning_token_count",
	"tool_token_count", "prompt_length", "user_prompt_length", "response_length", "duration_ms", "ttft_ms",
	"first_content_ms", "interaction.duration_ms", "busy_ns", "idle_ns",
	// Where in the code a span was opened: source locations and threads.
	"code.file.path", "code.line.number", "code.module.name", "target", "thread.name",
	// Classified from the first unclassified-key survey of every harness's withheld-content
	// run (2026-09-30), string-valued ones (numbers and booleans pass whatever their key).
	// Hooks, plugins, managed settings and skills.
	"hook_name", "hook_type", "hook_source", "hook_matcher", "hook_event", "hook_event_name", "handler_type",
	"enabled_via", "mode", "phase", "phases",
	"trigger", "stage", "kind", "outcome",
	// App servers, hooks, tracing and runtimes.
	"rpc.method", "rpc.request_id", "rpc.system", "rpc.transport", "submission.id", "turn_id",
	"hook.command_outcome", "hook.display_order", "hook.event_name", "hook.execution_mode", "hook.handler_type",
	"hook.scope", "hook.source", "hook.timeout_sec", "execution_mode", "environment_id", "unified_exec_process_id",
	"build_mode", "bundle_shape", "catalog_surface", "refresh_strategy", "tool_type", "tool_origin", "wire_api",
	"transport", "api.path", "startup.phase", "startup.status", "installation.id", "error_kind", "operation",
	"method", "provider", "prompt_id", "version", "level", "role", "format", "output_format", "mimetype",
	"language", "programming_language", "http.method", "server.address", "extension", "extensions",
	"extension_id", "extension_ids", "extension_name", "feature", "function_name", "mcp_tool", "mcp_tools",
	"mcp_server_name", "embedding_model", "auth_type", "approval_mode", "decision_model", "decision_source",
	"routing.decision_model", "routing.decision_source", "routing.approval_mode", "os_platform", "os_arch",
	"os_release", "finish_reasons", "start_time", "end_time", "model.provided",
	// OpenCode (terma's plugin) and Gemini CLI.
	"cwd", "gen_ai.tool.call_id", "gen_ai.token.type", "gen_ai.output.type",
	"gen_ai.prompt.name", "gen_ai.agent.name", "gen_ai.request.seed", "gen_ai.request.presence_penalty",
	"gen_ai.request.max_tokens", "gen_ai.request.frequency_penalty", "gen_ai.request.choice.count",
	"core_tools_enabled", "mcp_tools_count", "read_progress",
	// The process an exporter runs in — never its arguments (process.command_args).
	"process.pid", "process.owner", "process.command", "process.executable.name", "process.executable.path",
	"process.runtime.name", "process.runtime.version", "process.runtime.description",
	// Resource attributes.
	"service.name", "service.version", "host.arch", "host.name", "os.type", "os.version", "os", "os_version", "env",
	"telemetry.sdk.language", "telemetry.sdk.name", "telemetry.sdk.version",
)

// genericSafePrefixes are key families that are numbers by construction.
var genericSafePrefixes = []string{"gen_ai.usage."}

func setOf(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// contentKey reports whether key is one an agent declares carries content.
func (ru *rules) contentKey(key string) bool {
	return contains(ru.promptFields, key) || contains(ru.promptDropFields, key) || contains(ru.toolContentFields, key) ||
		contains(ru.resourcePromptFields, key)
}

// safeKey reports whether key is classified as never carrying content: safe, and not
// content for any agent, exactly or under a safe prefix. A number or a boolean under any
// key is safe too (content.go, scalarNonText); this is for strings.
func (ru *rules) safeKey(key string) bool {
	if ru.contentKey(key) {
		return false
	}
	if ru.safeKeys[key] {
		return true
	}
	for _, p := range ru.safePrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}
