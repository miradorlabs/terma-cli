package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// agy names no turn: `invocationNum` restarts each turn, `initialNumSteps` moves each
// invocation, and Stop's `executionNum` is per process. `initialNumSteps` at invocation 0
// only grows, and is the step the turn's first call comes after, so it names the turn.
//
// The record is the conversation's turn in progress and where a collecting hook saw it
// begin: one per conversation, written by the turn's first PreInvocation and removed by the
// turn's Stop, by any hook of the conversation that cannot tell where it ran or runs where
// the policy does not collect, and by a hook whose payload terma cannot read. After a Stop,
// a prompt or a wake starts at invocation 0 again. agy runs PreInvocation before it
// calls the model (on 1.5.15, a hook that slept 3 s delayed the call by 3 s), so the record
// names a turn before that turn's first call is written: a line read while the record still
// names a hook's turn is that turn's, or an earlier one's.

type antigravityTurn struct {
	TurnID string    `json:"turn_id"`
	At     time.Time `json:"at"`
	// Where is antigravityWhere of the hook that saw the turn begin.
	Where string `json:"where"`
}

func antigravityTurnPath(dir, sessionID string) string {
	return filepath.Join(dir, antigravityTurnDir, hookrun.EvidenceID(sessionID)+".json")
}

// antigravityWhere names where a collecting hook's calls go: its checkout's path, origin
// and project, and the lane it queues in, the sign-in in place or no person under a server
// key, hashed. A turn begun under one of them is never sent under another: a hook anywhere
// else, or signed in as someone else, finds the record is not its own.
func antigravityWhere(e hookrun.Env, r *hookrun.Repo) string {
	// The lane the hook's events queue in, as hookrun stamps them.
	l := spool.LaneOf(e.Account, e.ServerKey)
	if e.Spool != nil {
		l = e.Spool.Lane()
	}
	lane := "@" + l.Account
	if l.NoPerson {
		lane = "no_person"
	}
	return hookrun.EvidenceID(r.Root + "\x00" + r.Repository.Origin + "\x00" + r.ProjectID + "\x00" + lane)
}

// beginAntigravityTurn records the turn that starts at invocation 0, and where, and returns
// its id. With no repository, no `initialNumSteps` or a record it cannot write, it removes
// the previous turn's record rather than leave it standing, and returns "".
func beginAntigravityTurn(e hookrun.Env, r *hookrun.Repo, in *antigravityHookInput) string {
	path := antigravityTurnPath(e.StateDir, in.id())
	steps, _, ok := hookrun.JSONNumber(in.InitialNumSteps, true)
	if r == nil || !ok {
		_ = os.Remove(path)
		return ""
	}
	turn := antigravityTurn{TurnID: "turn-" + strconv.FormatUint(uint64(steps), 10), At: e.Time(), Where: antigravityWhere(e, r)}
	b, err := json.Marshal(turn)
	if err != nil {
		_ = os.Remove(path)
		return ""
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.Logf("antigravity turn: %v", err)
		_ = os.Remove(path)
		return ""
	}
	_, statErr := os.Stat(path)
	if err := hookrun.WriteState(path, b); err != nil {
		e.Logf("antigravity turn: %v", err)
		_ = os.Remove(path)
		return ""
	}
	if os.IsNotExist(statErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
	return turn.TurnID
}

// endAntigravityTurn removes the conversation's record: a hook where the policy collects
// nothing ends the turn terma was following, and no later hook sends the rest of it.
func endAntigravityTurn(e hookrun.Env, sessionID string) {
	if err := os.Remove(antigravityTurnPath(e.StateDir, sessionID)); err != nil && !os.IsNotExist(err) {
		e.Logf("antigravity turn: %v", err)
	}
}

// antigravityTurnID is the conversation's turn when a hook here, in this checkout and lane,
// saw it begin; "" when terma did not, or it began somewhere else.
func antigravityTurnID(e hookrun.Env, r *hookrun.Repo, sessionID string) string {
	if r == nil {
		return ""
	}
	b, err := os.ReadFile(antigravityTurnPath(e.StateDir, sessionID))
	if err != nil || len(b) > 4<<10 {
		return ""
	}
	var turn antigravityTurn
	if json.Unmarshal(b, &turn) != nil || !hookrun.ShortLabel(turn.TurnID) || turn.Where != antigravityWhere(e, r) {
		return ""
	}
	return turn.TurnID
}

// antigravityTurnNumber is the number in a turn id, -1 for none.
func antigravityTurnNumber(turn string) int64 {
	n, err := strconv.ParseInt(strings.TrimPrefix(turn, "turn-"), 10, 64)
	if err != nil || !strings.HasPrefix(turn, "turn-") {
		return -1
	}
	return n
}
