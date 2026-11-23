package antigravity

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Which hook may send a model call. This is the one statement of the rule; the code
// elsewhere points here.
//
// The record. agy names no turn: `invocationNum` restarts each turn, `initialNumSteps`
// moves each invocation, and Stop's `executionNum` is per process. But `initialNumSteps` at
// invocation 0 only grows, and it is the step the turn's first call comes after, so it
// names the turn. The record holds the conversation's turn in progress and where a
// collecting hook saw it begin: the checkout and the lane (antigravityWhere). There is one
// record per conversation. The turn's first PreInvocation writes it.
//
// A hook sends a call only when all three hold:
//   - the record names this hook's checkout and lane;
//   - the call's step is at or after the turn's first;
//   - the record still says so right before the send.
//
// These end a turn, each by removing the record. Nothing else does:
//   - the turn's Stop, once it has read the turn's last calls;
//   - a hook that runs where the policy collects nothing, or cannot tell where it ran;
//   - a PostInvocation at invocation 0 whose `initialNumSteps` is not the record's turn:
//     the record is stale, and this turn's first hook never ran;
//   - a hook whose payload terma cannot read;
//   - a hook run while hooks are off, switched off or torn down.
//
// What a hook passes and counts. A hook passes for good every call it reads and does not
// send: the calls of turns before terma watched the conversation, of a turn begun in
// another checkout or lane, and the last call of a cancelled turn, after which agy runs no
// hook of that turn. A hook in another checkout or lane leaves the record alone; the turn's
// own hooks send what comes after. A collecting hook counts the calls it passes as lost
// (turn_ended). A hook where the policy collects nothing reads nothing. A subagent's link
// passed the same way is not a call and is not counted: the subagent is then sent with no
// parent.
//
// A calls state that begins. When there is no saved read state, or it does not parse, the
// lines already in the transcript are passed, unsent and uncounted, however many hooks the
// read takes (antigravityCallState.Before). They were there before terma followed the
// conversation, or may have been sent already. Lines written after go by the rule above.

type antigravityTurn struct {
	TurnID string `json:"turn_id"`
	// At tells one beginning from another of the same number (endAntigravityTurnBegun).
	At time.Time `json:"at"`
	// Where is antigravityWhere of the hook that saw the turn begin.
	Where string `json:"where"`
}

func antigravityTurnPath(dir, sessionID string) string {
	return filepath.Join(dir, antigravityTurnDir, hookrun.EvidenceID(sessionID)+".json")
}

// antigravityWhere names, hashed, where a collecting hook's calls go: its checkout's path,
// origin and project, and the lane it queues in (the sign-in in place, or no person under a
// server key).
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

// endAntigravityTurn removes the conversation's record, which ends its turn: no later hook
// sends the rest of it.
func endAntigravityTurn(e hookrun.Env, sessionID string) {
	if err := os.Remove(antigravityTurnPath(e.StateDir, sessionID)); err != nil && !os.IsNotExist(err) {
		e.Logf("antigravity turn: %v", err)
	}
}

// endAntigravityTurnInEnvironment ends the turn of the conversation agy's environment
// names, for a hook that has only the environment to go by: one whose payload terma cannot
// read, or one run with hooks off, which reads no payload.
func endAntigravityTurnInEnvironment(e hookrun.Env) {
	if id := os.Getenv(antigravityConversationEnv); session.ValidID(id) {
		endAntigravityTurn(e, id)
	}
}

// antigravityTurnBegun is the conversation's record as it stands, nil for none: what a Stop
// reads first, to end the turn it is of and no other.
func antigravityTurnBegun(e hookrun.Env, sessionID string) []byte {
	b, _ := os.ReadFile(antigravityTurnPath(e.StateDir, sessionID))
	return b
}

// endAntigravityTurnBegun removes the record if it is still begun, the turn a Stop found
// when it started: a turn that began since keeps its own. The compare and the remove are
// two steps, which is enough because agy starts the next turn's first hook only once the
// Stop hook has returned (seen on 1.5.15 with a Stop hook that slept 6 s).
func endAntigravityTurnBegun(e hookrun.Env, sessionID string, begun []byte) {
	if begun != nil && bytes.Equal(antigravityTurnBegun(e, sessionID), begun) {
		endAntigravityTurn(e, sessionID)
	}
}

// endAntigravityTurnUnlessBegunAt ends the conversation's turn unless its record, whatever
// checkout or lane wrote it, names the turn that begins at steps: what a PostInvocation at
// invocation 0 gives, the same initialNumSteps its turn's first PreInvocation carried. It
// writes no record of its own, since no hook saw where this turn began: its calls are passed.
func endAntigravityTurnUnlessBegunAt(e hookrun.Env, sessionID string, steps int64) {
	b := antigravityTurnBegun(e, sessionID)
	if b == nil {
		return
	}
	var turn antigravityTurn
	if json.Unmarshal(b, &turn) != nil || antigravityTurnNumber(turn.TurnID) != steps {
		endAntigravityTurn(e, sessionID)
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
