// Package antigravity integrates Antigravity CLI (agy).
//
// agy has no telemetry export, so terma makes its events from four hooks and the
// conversation's transcript. How a hook becomes events:
//
//   - The hooks (handlers.go): PreInvocation, PostToolUse, PostInvocation and Stop. Each reads
//     its payload, resolves the checkout it ran in, and opens the conversation.
//   - antigravityConversation (conversation.go): the hook's one hold of the conversation's
//     lock, the read state saved between hooks, and the clock every event is stamped from.
//   - The reader (transcript_read.go): readAntigravitySteps hands on the lines written since
//     the last read; antigravityBatch knows which lines are tool results, and whether their
//     batch asked for a call whose content is refused.
//   - Which hook may send (turn.go): one of the turn's own checkout and lane, while the
//     turn's record stands. The rule is stated there once.
//   - Which content may go (content_paths.go): the developer's consent, the policy's switches,
//     every workspace collected, and for agy's own file and folder tools the checkout of each
//     path a call names; no other tool's arguments are judged.
//   - What is sent (call.go, content.go): a model call, a prompt, a reply and a tool's result,
//     cut to size. transcript.go's send picks them for a line; PostToolUse sends the tool
//     call with its arguments.
package antigravity

import (
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Agent is Google's Antigravity CLI (`agy`), with machine-wide hooks in the global
// customization root of each Antigravity home: agy's own, ~/.gemini/config/hooks.json, and
// each one T3 Code keeps for the agy it runs (homes.go). Its status line is in agy's
// settings, ~/.gemini's only; ConfigDir is terma's, where setup records what it replaced
// there.
type Agent struct{ ConfigDir string }

func (Agent) Name() string        { return "antigravity" }
func (Agent) DisplayName() string { return "Antigravity" }

// Surfaces is Antigravity as one choice, marked beta at setup and naming the first agy
// terma sets up.
func (a Agent) Surfaces() []agents.Surface {
	return []agents.Surface{{Name: a.Name(), DisplayName: a.DisplayName() + " " + minimumVersion + "+ (beta)", Installed: a.Installed}}
}

// Installed reports whether an agy terma can set up is on the PATH, or a client keeps an
// Antigravity home here, with a copy of agy of its own, whatever the agy on PATH is. An agy
// on PATH before minimumVersion counts for nothing: PlanUserHooks writes nothing into
// ~/.gemini for it, and with no client's home setup --yes leaves Antigravity out rather
// than stop at the refusal.
func (Agent) Installed(ctx context.Context) bool {
	if _, old := tooOld(ctx); !old && detect(ctx).Found {
		return true
	}
	clients, _ := clientHomes()
	return len(clients) > 0
}

// DetectedVersion is the version of the agy on PATH, for doctor; "" when it cannot tell.
func (Agent) DetectedVersion(ctx context.Context) string { return detect(ctx).Version }

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"antigravity-pre-invocation":  preInvocation,
		"antigravity-post-tool-use":   postToolUse,
		"antigravity-post-invocation": postInvocation,
		"antigravity-stop":            stop,
	}
}

func (Agent) FlushAfter() []string { return []string{"antigravity-stop"} }

// WhenHooksOff still answers each hook with the {} agy expects, with hooks switched off or
// terma torn down while a session runs. It also ends the turn of the conversation agy's
// environment names, since terma follows no turn while hooks are off (see turn.go).
func (a Agent) WhenHooksOff() map[string]agents.Handler {
	off := map[string]agents.Handler{}
	for event := range a.Events() {
		off[event] = func(_ context.Context, env hookrun.Env) error {
			antigravityAck(env)
			endAntigravityTurnInEnvironment(env)
			return nil
		}
	}
	return off
}

// Renders is the wrapped statusLine command; the renderer has its own deadline.
func (Agent) Renders() map[string]agents.RenderHandler {
	return map[string]agents.RenderHandler{antigravityStatusLineEvent: func(ctx context.Context, env hookrun.Env) int {
		renderer, err := statusLineRenderer(env.ConfigDir)
		if err != nil {
			env.Logf("status line record: %v", err)
		}
		return antigravityStatusLine(ctx, env, renderer, "")
	}}
}

// PayloadSession reads agy's protojson payload; the directory comes from it, since the
// hook runs in the folder holding hooks.json.
func (Agent) PayloadSession(payload []byte) (hookrun.PayloadSession, bool) {
	var in antigravityHookInput
	if json.Unmarshal(payload, &in) != nil || in.id() == "" {
		return hookrun.PayloadSession{}, false
	}
	return hookrun.PayloadSession{ID: in.id(), Cwd: in.cwd("")}, true
}

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportPartial, Note: "files its file tools write; a file a shell command writes is not recorded"},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "hooks and its transcript, no OTLP export: each model call's tokens (thinking not counted apart), every turn and tool step; under the team's content policy, each prompt, reply, tool call's arguments and tool's result, cut at 16 KiB; no thinking; plan, quota and account from the status line, interactive agy sessions only, so none from T3 Code threads; the version of the running agy or of T3 Code's own copy, none when terma cannot tell or on Windows; no cost or tool durations"}
}

// ContentConsented is the consent prompts, replies, tool arguments and results travel under.
func (Agent) ContentConsented(c hookrun.Consent) bool { return contentConsented(c) }

var (
	_ agents.Covered          = Agent{}
	_ agents.StateKeeper      = Agent{}
	_ agents.Agent            = Agent{}
	_ agents.PayloadReader    = Agent{}
	_ agents.UserHooks        = Agent{}
	_ agents.UserHooksFiles   = Agent{}
	_ agents.Surfaced         = Agent{}
	_ agents.OffSwitched      = Agent{}
	_ agents.VersionReporter  = Agent{}
	_ agents.MachineRefresher = Agent{}
	_ agents.StatusLiner      = Agent{}
	_ agents.Renderer         = Agent{}
	_ agents.ContentConsent   = Agent{}
)

// StateDirs are the directories its hooks keep state in.
func (Agent) StateDirs() []string {
	return []string{antigravityObservationDir, antigravityTurnDir, antigravityCallDir, antigravityStatusLineDir, antigravitySkillStateDir, antigravityVersionDir}
}
