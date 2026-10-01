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

// subagentStart announces a thread the session spawned with the spawn record from
// the child's rollout, rollout_status saying whether it was read.
func subagentStart(ctx context.Context, env hookrun.Env) error {
	return codexSubagent(ctx, env, hookrun.EventSubagentStart)
}

// subagentStop handles Codex's per-turn subagent end, told apart by turn_id.
func subagentStop(ctx context.Context, env hookrun.Env) error {
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
		spawn, status := rolloutSpawn(ctx, in.AgentID, in.TranscriptPath)
		attrs["rollout_status"] = status
		if status == hookrun.StatusPresent {
			codexSpawnAttrs(attrs, hookrun.AttrAgentParentID, spawn)
		}
	}
	env.EmitFor(r, spool.Event{Name: name, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
	return nil
}

// codexRolloutID is the thread whose rollout transcript_path names: a subagent's points at
// the child's own rollout, which the confined open refuses as the session's. Codex names a
// rollout file after its thread.
func codexRolloutID(in *codexHookInput) string {
	if session.ValidID(in.AgentID) && strings.Contains(filepath.Base(in.TranscriptPath), in.AgentID) {
		return in.AgentID
	}
	return in.SessionID
}

// codexSpawnAttrs copies the spawning thread (under parentKey) and Codex's labels for the
// child, a random nickname and a "/root/<task>" path.
func codexSpawnAttrs(attrs map[string]any, parentKey string, spawn threadSpawn) {
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
