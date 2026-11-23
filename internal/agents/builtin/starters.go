package builtin

import "github.com/miradorlabs/terma-cli/internal/agents"

// grokBuild is xAI's grok CLI, which terma has no agent for yet. grok also runs the entries
// in Claude Code's and Cursor's hooks files, and sets GROK_HOOK_EVENT on every hook process
// it starts, and on those alone: the commands its shell tool runs get GROK_SESSION_ID but
// not this (grok 1.0.92). This rule moves to grok's own agent package, which then
// implements agents.HookStarter, once that package exists.
type grokBuild struct{}

var _ agents.HookStarter = grokBuild{}

// StartedHook reports whether grok started this hook process.
func (grokBuild) StartedHook(getenv func(string) string) bool {
	return getenv("GROK_HOOK_EVENT") != ""
}
