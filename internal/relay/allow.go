package relay

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// With content withheld, only attribute keys classified safe leave: any other key is
// dropped and counted as unclassified.<key>, so a field a new agent release adds is a
// visible loss, never a leak. The composed set is pinned in this package's tests.

// genericSafeKeys are the safe keys no one agent owns; an agent's own are its shape.CaptureRules.SafeKeys.
var genericSafeKeys = setOf(
	// Identity and correlation.
	"session.id", "gen_ai.conversation.id",
	"tool_use_id", "gen_ai.tool.call.id", "gen_ai.response.id",
	"event.name", "event.timestamp",
	semconv.MiradorProjectIDKey, semconv.TermaRelayAttributionKey, semconv.TermaRelaySessionIDKey, semconv.TermaAccountSeatIDKey,
	// Models, providers and settings.
	"model", "gen_ai.request.model", "gen_ai.response.model", "gen_ai.system", "gen_ai.provider.name",
	"gen_ai.operation.name", "gen_ai.response.finish_reasons",
	"terminal.type", "source",
	"endpoint", "http.response.status_code", "status", "success", "decision", "attempt",
	// Tools, by name and shape — never their input or output.
	"tool_name", "tool", "gen_ai.tool.name",
	// Counts, sizes, costs and timings.
	"input_tokens", "output_tokens", "cache_read_tokens",
	"prompt_length", "response_length", "duration_ms", "ttft_ms",
	// Where in the code a span was opened: source locations and threads.
	"code.file.path", "code.line.number", "code.module.name", "thread.name",
	// Hooks, plugins, managed settings and skills.
	"hook_name", "hook_type", "hook_source", "hook_event",
	"mode", "phase", "phases",
	"trigger", "stage", "kind", "outcome",
	// App servers, hooks, tracing and runtimes.
	"rpc.method", "rpc.request_id", "rpc.system", "rpc.transport",
	"environment_id",
	"transport", "error_kind",
	"method", "provider", "version", "level", "role", "format",
	"language", "http.method", "server.address",
	"feature", "mcp_tool",
	"os_platform", "os_release", "start_time", "end_time",
	// Semantic-convention gen_ai attributes, and exporter settings.
	"cwd", "gen_ai.tool.call_id", "gen_ai.token.type", "gen_ai.output.type",
	"gen_ai.prompt.name", "gen_ai.agent.name", "gen_ai.request.seed", "gen_ai.request.presence_penalty",
	"gen_ai.request.max_tokens", "gen_ai.request.frequency_penalty", "gen_ai.request.choice.count",
	// The process an exporter runs in — never its arguments (process.command_args).
	"process.pid", "process.owner", "process.command", "process.executable.name", "process.executable.path",
	"process.runtime.name", "process.runtime.version", "process.runtime.description",
	// Resource attributes.
	"service.name", "service.version", "host.arch", "host.name", "os.type", "os.version",
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

func (ru *rules) contentKey(key string) bool {
	return contains(ru.promptFields, key) || contains(ru.promptDropFields, key) || contains(ru.toolContentFields, key) ||
		contains(ru.resourcePromptFields, key)
}

// safeKey is for strings: a key that is content for any agent is never safe.
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
