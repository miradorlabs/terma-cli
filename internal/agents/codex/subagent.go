package codex

import (
	"cmp"
	"context"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// CodexSubagentStart announces a Codex subagent, which is a thread the session spawned.
// Codex sends the root thread's session_id, the child thread's id as agent_id, and the
// child's own rollout as transcript_path (codex-rs/core/src/hook_runtime.rs, read
// 2026-09-17; not yet seen live). The rollout's first line is the spawn record — which
// thread spawned this one, how deep, and Codex's labels for it — and the start carries
// it, with rollout_status saying whether it could be read rather than dropping a miss.
//
// SubagentStop fires at the end of every turn of the child thread, not once: a Codex
// end is per turn, told apart by turn_id, where Claude Code's is a bracket.
func CodexSubagentStart(ctx context.Context, env hookrun.Env) error {
	return codexSubagent(ctx, env, hookrun.EventSubagentStart)
}

// CodexSubagentStop is the end of the subagent CodexSubagentStart announced.
func CodexSubagentStop(ctx context.Context, env hookrun.Env) error {
	return codexSubagent(ctx, env, hookrun.EventSubagentEnd)
}

func codexSubagent(ctx context.Context, env hookrun.Env, name string) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) || !session.ValidID(in.AgentID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	attrs := hookrun.AgentAttrs(map[string]any{hookrun.AttrTool: codexTool, hookrun.AttrSchemaVersion: 1, hookrun.AttrModel: in.Model}, in.AgentID, in.AgentType)
	if session.ValidID(in.TurnID) {
		attrs[hookrun.AttrTurnID] = in.TurnID
	}
	if name == hookrun.EventSubagentStart {
		attrs[hookrun.AttrVersion] = env.Version
		spawn, status := CodexRolloutSpawn(ctx, in.AgentID, in.TranscriptPath)
		attrs["rollout_status"] = status
		if status == hookrun.StatusPresent {
			codexSpawnAttrs(attrs, hookrun.AttrAgentParentID, spawn)
		}
	}
	env.EmitFor(r, spool.Event{Name: name, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
	return nil
}

// codexRolloutID is the thread whose rollout a Codex hook's transcript_path names. A
// subagent in Codex is a spawned thread with a rollout of its own: its hooks keep the
// root thread's session_id, put the child thread's id in agent_id, and point
// transcript_path at the child's rollout. Reading that file as the session's fails the
// confined open's own check — the rollout says it is another thread's — so capture
// inside a subagent recorded nothing but a mismatch. Codex names a rollout file after
// its thread, which is what settles it; anything else is the session's, as before.
func codexRolloutID(in *codexHookInput) string {
	if session.ValidID(in.AgentID) && strings.Contains(filepath.Base(in.TranscriptPath), in.AgentID) {
		return in.AgentID
	}
	return in.SessionID
}

// codexSpawnAttrs copies a rollout's spawn record: the thread that spawned this one,
// under parentKey, and Codex's own labels for the child (a random nickname, a
// "/root/<task>" path). The parent is a session when the child is one (a start event
// of its own) and an agent's parent when the child is a facet of the root's session.
func codexSpawnAttrs(attrs map[string]any, parentKey string, spawn CodexThreadSpawn) {
	if !session.ValidID(spawn.ParentThreadID) {
		return
	}
	attrs[parentKey] = spawn.ParentThreadID
	if spawn.Depth > 0 {
		attrs["agent_depth"] = spawn.Depth
	}
	if hookrun.ShortLabel(spawn.AgentNickname) {
		attrs["agent_nickname"] = spawn.AgentNickname
	}
	if hookrun.ShortLabel(spawn.AgentPath) {
		attrs["agent_path"] = spawn.AgentPath
	}
}
