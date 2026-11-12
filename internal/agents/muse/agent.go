// Package muse integrates Muse Code (`muse`), Meta's terminal coding agent.
package muse

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// name is the agent's id, and gen_ai.main_agent.name on what its hooks spool.
const name = "muse"

// Agent is Muse Code. Its hooks, in its user-level settings.json, are terma's only window
// on it: its OTLP export goes to Meta alone, switching itself off when pointed anywhere
// else, and carries no prompt or tool content.
type Agent struct{}

func (Agent) Name() string        { return name }
func (Agent) DisplayName() string { return "Muse Code" }
func (Agent) Installed(context.Context) bool {
	_, err := exec.LookPath("muse")
	return err == nil
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"muse-session-start":  sessionStart,
		"muse-prompt":         prompt,
		"muse-post-tool-use":  postToolUse,
		"muse-post-llm-call":  postLLMCall,
		"muse-subagent-start": subagentStart,
		"muse-subagent-stop":  subagentStop,
		"muse-stop":           stop,
		"muse-session-end":    sessionEnd,
		museFeedEvent:         feed,
	}
}

func (Agent) FlushAfter() []string { return []string{"muse-stop", "muse-session-end", museFeedEvent} }

// PayloadSession names a hook's session by the root's id: a subagent's hooks carry the
// child's as session_id, and Muse sets MUSE_SESSION_ID to the root in every hook.
func (Agent) PayloadSession(payload []byte) (hookrun.PayloadSession, bool) {
	var in museHookInput
	if json.Unmarshal(payload, &in) != nil {
		return hookrun.PayloadSession{}, false
	}
	root, thread := in.ids()
	if root == "" {
		return hookrun.PayloadSession{}, false
	}
	return hookrun.PayloadSession{ID: root, AgentID: thread, Cwd: in.Cwd}, true
}

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "hooks only: prompts, replies, every tool call and each model call's tokens; no cost, plan or quota — Muse's OTLP export goes to Meta alone"}
}

// UserHooksPath is Muse's user-level settings, where it reads its hooks.
func (Agent) UserHooksPath() (string, error) {
	dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "muse", museSettings), nil
}

func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	return planHooks(dir, command, install)
}

// HooksRunInCleanEnv: Muse hands a hook only HOME, PATH and a few others (measured on
// 1.4.14), never TERMA_ or XDG_ variables, so its entries name terma's directories.
func (Agent) HooksRunInCleanEnv() {}

// ContentConsented is delivery asking again, as each prompt and reply leaves, whether the
// developer chose Muse; the team's policy decides the rest.
func (Agent) ContentConsented(c hookrun.Consent) bool { return consented(c) }

var (
	_ agents.StateKeeper    = Agent{}
	_ agents.ContentConsent = Agent{}
	_ agents.Covered        = Agent{}
	_ agents.PayloadReader  = Agent{}
	_ agents.UserHooks      = Agent{}
	_ agents.CleanHookEnv   = Agent{}
)
