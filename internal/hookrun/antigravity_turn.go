package hookrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// agy names no turn. A turn is one run of its execution loop — the person's message, the
// model calls and tool steps it takes, the Stop — and no hook payload identifies it:
// PostToolUse carries a step index and nothing else, Pre/PostInvocation carry
// `invocationNum` (which restarts at 0 every turn) and `initialNumSteps` (which moves with
// every invocation, not every turn), and Stop's `executionNum` counts executions of the
// *process*, so a resumed conversation's second turn reports 0 again. Live-checked on agy
// 1.2.7, 2026-09-18, over two turns of one conversation.
//
// What does identify a turn is where it began: `initialNumSteps` at invocation 0 is the
// length of the trajectory when the person's message arrived. It only grows, so it is
// unique within the conversation, the same under replay, and independent of whether terma
// saw the conversation's earlier turns. PreInvocation records it; every later hook of the
// turn — each its own process — reads it back.

type antigravityTurn struct {
	TurnID string    `json:"turn_id"`
	At     time.Time `json:"at"`
}

func antigravityTurnPath(sessionID, root string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, antigravityTurnDir, evidenceID(sessionID+"\x00"+root)+".json"), nil
}

// beginAntigravityTurn records the turn that starts at invocation 0 and returns its id.
// A payload without `initialNumSteps` starts a turn terma cannot name: the previous
// turn's record is removed rather than left to label this one's events.
func (e Env) beginAntigravityTurn(r *repo, in *antigravityHookInput) string {
	path, err := antigravityTurnPath(in.id(), r.root)
	if err != nil {
		return ""
	}
	steps, _, ok := cursorNumber(in.InitialNumSteps, true)
	if !ok {
		_ = os.Remove(path)
		return ""
	}
	turn := antigravityTurn{TurnID: "turn-" + strconv.FormatUint(uint64(steps), 10), At: e.now()}
	b, err := json.Marshal(turn)
	if err != nil {
		return ""
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.logf("antigravity turn: %v", err)
		return ""
	}
	_, statErr := os.Stat(path)
	if err := writeState(path, b); err != nil {
		e.logf("antigravity turn: %v", err)
		return ""
	}
	if os.IsNotExist(statErr) {
		pruneQuotaState(dir, e.now().Add(-spool.MaxAge))
	}
	return turn.TurnID
}

// antigravityTurnID is the turn the conversation is in, or "" when terma did not see it
// begin (hooks installed mid-turn, a pruned record). Missing stays missing.
func antigravityTurnID(r *repo, sessionID string) string {
	path, err := antigravityTurnPath(sessionID, r.root)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 4<<10 {
		return ""
	}
	var turn antigravityTurn
	if json.Unmarshal(b, &turn) != nil || !shortLabel(turn.TurnID) {
		return ""
	}
	return turn.TurnID
}
