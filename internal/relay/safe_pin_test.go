package relay

import (
	"maps"
	"slices"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// pinnedSafeKeys and pinnedSafePrefixes are every key that may leave a machine with content
// withheld; written by hand, so widening them is a reviewed edit.
var pinnedSafeKeys = []string{
	"agent_name", "api.path", "app.version", "app_server.api_version",
	"app_server.client_name", "app_server.client_version", "app_server.connection_id",
	"approval_mode", "approval_policy", "attempt", "auth.env_codex_api_key_enabled",
	"auth.env_codex_api_key_present", "auth.env_openai_api_key_present",
	"auth.env_refresh_token_url_override_present", "auth.header_attached",
	"auth.retry_after_unauthorized", "auth_mode", "auth_type", "bash_argv0",
	"bash_command_class", "build_mode", "bundle_shape", "busy_ns",
	"cache_creation_tokens", "cache_read_tokens", "cache_write_token_count",
	"cached_token_count", "call_id", "catalog_surface", "code.file.path",
	"code.line.number", "code.module.name", "codex.op", "codex.request.reasoning_effort",
	"codex.turn.reasoning_effort", "command_category", "conversation.id",
	"core_tools_enabled", "cost_usd", "cost_usd_micros", "cwd", "decision",
	"decision_model", "decision_source", "dsh.purpose", "dsh.turn", "duration_ms",
	"embedding_model", "enabled_via", "end_time", "endpoint", "env", "environment_id",
	"error_kind", "event.kind", "event.name", "event.sequence", "event.timestamp",
	"execution_mode", "extension", "extension_id", "extension_ids", "extension_name",
	"extensions", "feature", "finish_reasons", "first_content_ms", "format",
	"function_name", "gen_ai.agent.name", "gen_ai.conversation.id",
	"gen_ai.operation.name", "gen_ai.output.type", "gen_ai.prompt.name",
	"gen_ai.provider.name", "gen_ai.request.choice.count",
	"gen_ai.request.frequency_penalty", "gen_ai.request.max_tokens",
	"gen_ai.request.model", "gen_ai.request.presence_penalty", "gen_ai.request.seed",
	"gen_ai.response.finish_reasons", "gen_ai.response.id", "gen_ai.response.model",
	"gen_ai.system", "gen_ai.token.type", "gen_ai.tool.call.id", "gen_ai.tool.call_id",
	"gen_ai.tool.name", "handler_type", "hermes.turn_id", "hook.command_outcome",
	"hook.display_order", "hook.event_name", "hook.execution_mode", "hook.handler_type",
	"hook.scope", "hook.source", "hook.timeout_sec", "hook_event", "hook_event_name",
	"hook_matcher", "hook_name", "hook_source", "hook_type", "host.arch", "host.name",
	"http.method", "http.response.status_code", "idle_ns", "input_token_count",
	"input_tokens", "installation.id", "interaction.duration_ms", "interaction.sequence",
	"kind", "language", "level", "llm_request.context", "managed_settings.helper.applied",
	"managed_settings.helper.state", "managed_settings.source_behavior",
	"managed_settings.sources", "managed_settings.trigger", "marketplace.name",
	"mcp_server", "mcp_server_name", "mcp_server_origin", "mcp_servers", "mcp_tool",
	"mcp_tools", "mcp_tools_count", "message.uuid", "method", "mimetype",
	"mirador.project.id", "mode", "model", "model.provided", "model_reasoning_effort",
	"opencode.agent", "opencode.message.id", "opencode.parent_message.id",
	"opencode.project.id", "opencode.session.directory", "opencode.version", "operation",
	"originator", "os", "os.type", "os.version", "os_arch", "os_platform", "os_release",
	"os_version", "outcome", "output_format", "output_token_count", "output_tokens",
	"output_truncated", "parent.source", "phase", "phases", "plugin.name", "plugin.scope",
	"plugin_id_hash", "process.command", "process.executable.name",
	"process.executable.path", "process.owner", "process.pid",
	"process.runtime.description", "process.runtime.name", "process.runtime.version",
	"programming_language", "prompt.id", "prompt_id", "prompt_length", "provider",
	"provider_name", "query_source", "query_source_safe", "queued_sends", "read_progress",
	"reasoning_effort", "reasoning_summary", "reasoning_token_count", "refresh_strategy",
	"request_id", "response_length", "role", "routing.approval_mode",
	"routing.decision_model", "routing.decision_source", "rpc.method", "rpc.request_id",
	"rpc.system", "rpc.transport", "sandbox", "sandbox_policy", "server.address",
	"service.name", "service.version", "session.id", "session_source", "slug", "source",
	"span.type", "speed", "stage", "start_time", "start_type", "startup.phase",
	"startup.status", "status", "stop_reason", "submission.id", "success", "target",
	"telemetry.sdk.language", "telemetry.sdk.name", "telemetry.sdk.version",
	"terma.relay.attribution", "terma.relay.session.id", "terminal.type", "thread.id",
	"thread.name", "thread_id", "tmp_mem_enabled", "token_type", "tool",
	"tool_input_size_bytes", "tool_name", "tool_name_safe", "tool_namespace",
	"tool_origin", "tool_result_seq", "tool_result_size_bytes", "tool_source",
	"tool_token_count", "tool_type", "tool_use_id", "transport", "trigger", "ttft_ms",
	"turn.id", "turn_id", "type", "unified_exec_process_id", "user.id",
	"user_prompt_length", "version", "wire_api",
}

var pinnedSafePrefixes = []string{"codex.turn.token_usage.", "codex.usage.", "gen_ai.usage."}

func TestComposedSafeKeysArePinned(t *testing.T) {
	if got := slices.Sorted(maps.Keys(testRules.safeKeys)); !slices.Equal(got, pinnedSafeKeys) {
		added, removed := setDiff(got, pinnedSafeKeys), setDiff(pinnedSafeKeys, got)
		t.Errorf("the composed safe keys changed: added %q, removed %q", added, removed)
	}
	if got := slices.Sorted(slices.Values(testRules.safePrefixes)); !slices.Equal(got, pinnedSafePrefixes) {
		t.Errorf("the composed safe prefixes are %q, pinned %q", got, pinnedSafePrefixes)
	}
	for _, k := range pinnedSafeKeys {
		if !testRules.safeKey(k) {
			t.Errorf("pinned %q does not classify as safe: an agent declares it as content", k)
		}
	}
}

func setDiff(a, b []string) []string {
	return slices.DeleteFunc(slices.Clone(a), func(s string) bool { return slices.Contains(b, s) })
}

type capturer shape.CaptureRules

func (c capturer) CaptureRules() shape.CaptureRules { return shape.CaptureRules(c) }

// A key one agent declares safe and another content is content, whichever registers first.
func TestContentOutranksSafeAcrossAgents(t *testing.T) {
	safe := capturer{SafeKeys: []string{"x.detail"}, SafePrefixes: []string{"y."}}
	content := capturer{ToolContentFields: []string{"x.detail"}, PromptDropFields: []string{"y.prompt"}}
	for _, order := range [][]shape.Capturer{{safe, content}, {content, safe}} {
		ru := compose(nil, order)
		for key, want := range map[string]bool{"x.detail": false, "y.prompt": false, "y.count": true} {
			if got := ru.safeKey(key); got != want {
				t.Errorf("safeKey(%q) = %v, want %v", key, got, want)
			}
		}
	}
}
