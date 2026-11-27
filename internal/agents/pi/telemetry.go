package pi

import "github.com/miradorlabs/terma-cli/internal/relay/shape"

// scope is the instrumentation scope of everything terma's extension exports.
const scope = "terma-pi"

// Correlation names the extension's session: session.id, on every span and record. The key
// is Claimed on the extension's scope: the extension claims each session through its first
// hook before it exports anything of it, a nested session's too, so global mode's catch-all
// waits for the claim rather than sending a session's first batch to the default project.
func (Agent) Correlation() shape.Correlation {
	key := shape.SessionID
	key.Claimed, key.Scope = true, scope
	return shape.Correlation{SessionKeys: []shape.SessionKey{key}}
}

// CaptureRules are where the extension's export carries content beyond the generic rules (the
// reply in gen_ai.completion, a tool's arguments and result), and the string keys of its own
// that carry none.
func (Agent) CaptureRules() shape.CaptureRules {
	return shape.CaptureRules{
		// The prompt and the name a person gives a session, which follow the prompt switch
		// alone. pi.custom_message is deliberately not listed: an extension can put anything
		// in one, a file's content included, so its body leaves only when both are collected.
		PromptBodyEvents: []string{"pi.user_prompt", "pi.session.title"},
		// A provider's error can quote a prompt or a tool's arguments, and Pi appends the files
		// a session read and changed to every compaction summary: each leaves only when both
		// prompts and tool content are collected.
		PromptDropFields: []string{"pi.error.message", "pi.compaction.summary"},
		// The same two, and the files a file tool named (a list of absolute paths).
		ToolContentFields: []string{"pi.error.message", "pi.compaction.summary", "pi.tool.files"},
		// Strings only: a number or a flag passes whatever its key (tokens, cost, lengths, an
		// exit code, the tokens before a compaction, whether a provider billed the cost).
		SafeKeys: []string{
			// The build and how it ran (tui, print, json, rpc).
			"pi.version", "pi.mode",
			// Ids: the session a child process works for, the one a fork came from, the prompt
			// that opened a turn, a model call, and a session entry. A turn, a call or a parent
			// link lost to withholding would lose the run it belongs to.
			"pi.parent_session.id", "pi.session.forked_from.id", "pi.turn.id", "pi.call.id", "pi.entry.id",
			// Pi's own vocabularies: what a call outside the conversation was (compaction,
			// cache_warm), the thinking level, why a session was compacted (manual, threshold,
			// overflow), and how a tool call ended (ok, error, not_run).
			"pi.call.kind", "pi.thinking_level", "pi.compaction.reason", "pi.tool.outcome",
			// MCP servers and tools by name, as gen_ai.tool.name names a tool.
			"pi.mcp.server", "pi.mcp.tool",
		},
	}
}

var (
	_ shape.Correlator = Agent{}
	_ shape.Capturer   = Agent{}
)
