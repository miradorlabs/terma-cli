package relay

import "strings"

// With a project's content withheld, a record leaves with the attributes known to say
// nothing of what was said, and no others. Removing the content fields the relay knows
// (content.go) is not enough on its own: a harness release that adds one — Gemini CLI's
// process.command_args, which carries a -p prompt whatever logPrompts says — would leak
// until someone noticed. So every attribute key is classified. A content key keeps the
// treatment content.go gives it (a marker, or dropped); a key in safeKeys passes; any
// other key is dropped, and counted by name (Stats: unclassified.<key>) so that the
// live suite fails on it and a person decides which it is. A new harness field is then
// a visible loss, never a leak.
//
// The list is what the harnesses were seen to send with content withheld (the live
// goldens, live/golden/*/telemetry-redacted.json and golden/relay/*-withheld.json) and
// what terma's own exporters send (Pi's, omp's, Hermes's, DeepSeek Harness's and the
// OpenCode plugin), minus every key that carries content anywhere. A key that is safe in
// one harness and content in another is content: the list is one set for all of them.

// safeKeys are attribute keys that never carry what was said, in any harness.
var safeKeys = setOf(
	// Identity and correlation.
	"session.id", "conversation.id", "thread.id", "thread_id", "gen_ai.conversation.id", "prompt.id", "request_id",
	"message.uuid", "turn.id", "call_id", "tool_use_id", "gen_ai.tool.call.id", "gen_ai.response.id", "user.id",
	"event.name", "event.kind", "event.sequence", "event.timestamp", "interaction.sequence", "span.type",
	"mirador.project.id", "terma.relay.attribution", "terma.relay.session.id",
	// Models, providers and settings.
	"model", "gen_ai.request.model", "gen_ai.response.model", "gen_ai.system", "gen_ai.provider.name",
	"gen_ai.operation.name", "gen_ai.response.finish_reasons", "provider_name", "slug", "speed", "stop_reason",
	"reasoning_effort", "reasoning_summary", "model_reasoning_effort", "codex.request.reasoning_effort",
	"codex.turn.reasoning_effort", "approval_policy", "sandbox_policy", "sandbox", "auth_mode", "originator",
	"app.version", "terminal.type", "session_source", "source", "start_type", "query_source", "query_source_safe",
	"parent.source", "tool_source", "mcp_server", "mcp_server_origin", "mcp_servers", "agent_name", "tmp_mem_enabled",
	"auth.env_codex_api_key_enabled", "auth.env_codex_api_key_present", "auth.env_openai_api_key_present",
	"auth.env_refresh_token_url_override_present", "auth.header_attached", "auth.retry_after_unauthorized",
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
	// Where in the code a span was opened (Codex's tracing): source locations and threads.
	"code.file.path", "code.line.number", "code.module.name", "target", "thread.name",
	// terma's own exporters.
	"dsh.turn", "dsh.purpose", "hermes.turn_id",
	// Classified from the first unclassified-key survey of every harness's withheld-content
	// run (2026-09-30), string-valued ones (numbers and booleans pass whatever their key).
	// Claude Code: hooks, plugins, managed settings, skills.
	"hook_name", "hook_type", "hook_source", "hook_matcher", "hook_event", "hook_event_name", "handler_type",
	"plugin.name", "plugin.scope", "plugin_id_hash", "marketplace.name", "enabled_via", "mode", "phase", "phases",
	"managed_settings.trigger", "managed_settings.sources", "managed_settings.source_behavior",
	"managed_settings.helper.state", "managed_settings.helper.applied", "trigger", "stage", "kind", "outcome",
	// Codex: its app server, hooks, tracing and runtime.
	"app_server.api_version", "app_server.client_name", "app_server.client_version", "app_server.connection_id",
	"rpc.method", "rpc.request_id", "rpc.system", "rpc.transport", "codex.op", "submission.id", "turn_id",
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
	"opencode.version", "opencode.agent", "opencode.message.id", "opencode.parent_message.id", "opencode.project.id",
	"opencode.session.directory", "cwd", "gen_ai.tool.call_id", "gen_ai.token.type", "gen_ai.output.type",
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

// safePrefixes are key families that are numbers by construction.
var safePrefixes = []string{"gen_ai.usage.", "codex.turn.token_usage.", "codex.usage."}

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

// safeKey reports whether key is classified as never carrying content. A number or a
// boolean under any key is safe too (content.go, scalarNonText); this is for strings.
func safeKey(key string) bool {
	if safeKeys[key] {
		return true
	}
	for _, p := range safePrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// classify says how the relay treats key when a project's content is withheld: "safe"
// (passes), "content" (marked or dropped) or "unclassified" (dropped, and counted so that
// someone classifies it).
func (ru *rules) classify(key string) string {
	switch {
	case ru.contentKey(key):
		return "content"
	case safeKey(key):
		return "safe"
	}
	return "unclassified"
}
