package codex

import (
	"bytes"
	"context"
	"encoding/json"
)

// CodexThreadSpawn is session_meta.payload.source.subagent.thread_spawn, the parent link
// of a spawned thread; root and review threads have none.
type CodexThreadSpawn struct {
	ParentThreadID string
	Depth          int
	AgentNickname  string
	AgentPath      string
}

// CodexRolloutSpawn reads the rollout's first line through the confined open. Status is
// "present", "root" when not spawned, or the open failure.
func CodexRolloutSpawn(ctx context.Context, sessionID, transcript string) (CodexThreadSpawn, string) {
	f, status := openCodexRollout(ctx, sessionID, transcript)
	if f == nil {
		return CodexThreadSpawn{}, status
	}
	defer f.Close()
	head := make([]byte, 64<<10)
	n, _ := f.ReadAt(head, 0)
	line, _, _ := bytes.Cut(head[:n], []byte{'\n'})
	var meta struct {
		Payload struct {
			Source json.RawMessage `json:"source"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil {
		return CodexThreadSpawn{}, "unreadable"
	}
	var source struct {
		Subagent json.RawMessage `json:"subagent"`
	}
	if json.Unmarshal(meta.Payload.Source, &source) != nil || len(source.Subagent) == 0 {
		return CodexThreadSpawn{}, "root"
	}
	var sub struct {
		ThreadSpawn *struct {
			ParentThreadID string `json:"parent_thread_id"`
			Depth          int    `json:"depth"`
			AgentNickname  string `json:"agent_nickname"`
			AgentPath      string `json:"agent_path"`
		} `json:"thread_spawn"`
	}
	if json.Unmarshal(source.Subagent, &sub) != nil || sub.ThreadSpawn == nil || sub.ThreadSpawn.ParentThreadID == "" {
		return CodexThreadSpawn{}, "root"
	}
	return CodexThreadSpawn{
		ParentThreadID: sub.ThreadSpawn.ParentThreadID,
		Depth:          sub.ThreadSpawn.Depth,
		AgentNickname:  sub.ThreadSpawn.AgentNickname,
		AgentPath:      sub.ThreadSpawn.AgentPath,
	}, "present"
}
