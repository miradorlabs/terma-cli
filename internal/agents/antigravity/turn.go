package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// agy names no turn: `invocationNum` restarts each turn, `initialNumSteps` moves each
// invocation, and Stop's `executionNum` is per process. `initialNumSteps` at invocation 0
// only grows, so it names the turn; PreInvocation records it and the turn's later hooks
// read it back.

type antigravityTurn struct {
	TurnID string    `json:"turn_id"`
	At     time.Time `json:"at"`
}

func antigravityTurnPath(dir, sessionID, root string) string {
	return filepath.Join(dir, antigravityTurnDir, hookrun.EvidenceID(sessionID+"\x00"+root)+".json")
}

// beginAntigravityTurn records the turn that starts at invocation 0 and returns its id;
// without `initialNumSteps` it removes the previous turn's record rather than reuse it.
func beginAntigravityTurn(e hookrun.Env, r *hookrun.Repo, in *antigravityHookInput) string {
	path := antigravityTurnPath(e.StateDir, in.id(), r.Root)
	steps, _, ok := hookrun.JSONNumber(in.InitialNumSteps, true)
	if !ok {
		_ = os.Remove(path)
		return ""
	}
	turn := antigravityTurn{TurnID: "turn-" + strconv.FormatUint(uint64(steps), 10), At: e.Time()}
	b, err := json.Marshal(turn)
	if err != nil {
		return ""
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.Logf("antigravity turn: %v", err)
		return ""
	}
	_, statErr := os.Stat(path)
	if err := hookrun.WriteState(path, b); err != nil {
		e.Logf("antigravity turn: %v", err)
		return ""
	}
	if os.IsNotExist(statErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
	return turn.TurnID
}

// antigravityTurnID is the turn the conversation is in, or "" when terma did not see it begin.
func antigravityTurnID(e hookrun.Env, r *hookrun.Repo, sessionID string) string {
	b, err := os.ReadFile(antigravityTurnPath(e.StateDir, sessionID, r.Root))
	if err != nil || len(b) > 4<<10 {
		return ""
	}
	var turn antigravityTurn
	if json.Unmarshal(b, &turn) != nil || !hookrun.ShortLabel(turn.TurnID) {
		return ""
	}
	return turn.TurnID
}
