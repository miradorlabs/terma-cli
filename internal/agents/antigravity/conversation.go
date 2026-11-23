package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// antigravityConversation is one hook's hold of a conversation's calls state, under its lock,
// taken once per hook: how far the transcript was read, the steps whose content a hook
// refused, and the clock every event of the conversation is stamped from. A hook that cannot
// have the lock sends what it sends without it, at the real time, and no content.
type antigravityConversation struct {
	e   hookrun.Env
	r   *hookrun.Repo
	in  *antigravityHookInput
	ctx context.Context

	// held is that the lock is held and state is the conversation's.
	held   bool
	unlock func()
	path   string
	state  antigravityCallState
	// began is a state that begins (see turn.go); before is the state as loaded.
	began   bool
	before  []byte
	created bool

	content *antigravityContent
	// callStep is the step of the call a PostToolUse spools, -1 for none; callAt is its time,
	// stamped just before its result when the read meets that first.
	callStep int64
	callAt   time.Time
}

// openAntigravityConversation takes the conversation's lock, waiting up to
// antigravityCallLockWait for another of its hooks, and loads its calls state. Without a
// transcript of agy's shape, a spool or a safe id, it holds nothing.
func openAntigravityConversation(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *antigravityHookInput) *antigravityConversation {
	c := &antigravityConversation{e: e, r: r, in: in, ctx: ctx, callStep: -1}
	transcript := antigravityTranscript(in)
	id := in.id()
	if transcript == "" || e.Spool == nil || !session.ValidID(id) {
		return c
	}
	dir := filepath.Join(e.StateDir, antigravityCallDir)
	if os.MkdirAll(dir, 0o700) != nil {
		return c
	}
	c.path = filepath.Join(dir, hookrun.EvidenceID(id)+".json")
	lockCtx, cancel := context.WithTimeout(ctx, antigravityCallLockWait)
	defer cancel()
	unlock, err := flock.Lock(lockCtx, c.path+".lock")
	if err != nil {
		e.Logf("antigravity calls: %v", err)
		return c
	}
	c.held, c.unlock = true, unlock
	// A state that begins passes what the transcript already holds (see turn.go), and keeps
	// no refusal or clock of an older one.
	c.state = antigravityCallState{Step: -1}
	b, readErr := os.ReadFile(c.path)
	c.created = os.IsNotExist(readErr)
	c.began = readErr != nil || json.Unmarshal(b, &c.state) != nil
	if c.began {
		c.state = antigravityCallState{Step: -1}
		if info, err := os.Stat(transcript); err == nil {
			c.state.Before = info.Size()
		}
	}
	c.before, _ = json.Marshal(c.state)
	return c
}

// stamp is the time an event of the conversation leaves with: t, raised to 1 ms past the
// last time stamped when it is not already later, so the conversation's events sort in the
// order its hooks stamp them. Without the lock it is t.
func (c *antigravityConversation) stamp(t time.Time) time.Time {
	if !c.held {
		return t
	}
	ms := t.UnixMilli()
	if ms <= c.state.Clock {
		ms = c.state.Clock + 1
	}
	c.state.Clock = ms
	return time.UnixMilli(ms).UTC()
}

// now is stamp at the hook's real time.
func (c *antigravityConversation) now() time.Time { return c.stamp(c.e.Time()) }

// at is env with its clock set to the next stamped time, for the hookrun helpers that spool
// at env.Time().
func (c *antigravityConversation) at(env hookrun.Env) hookrun.Env {
	env.Now = c.now()
	return env
}

// callTime is the time of the call this PostToolUse spools: the one stamped before its
// result, when the read met that, else the next. A result never sorts before its call, and
// the model call that asked for the tool sorts before both.
func (c *antigravityConversation) callTime() time.Time {
	if !c.callAt.IsZero() {
		return c.callAt
	}
	return c.now()
}

// contentFor is the content this hook may attach, worked out once.
func (c *antigravityConversation) contentFor() antigravityContent {
	if c.content == nil {
		content := antigravityContentFor(c.ctx, c.e, c.in)
		c.content = &content
	}
	return *c.content
}

// refuse records that step's content is refused, before the transcript is read, so the
// reader withholds its result. It reports false when it cannot be recorded (the lock is not
// held): the caller then ends the turn, so the result cannot go through.
func (c *antigravityConversation) refuse(step int64) bool {
	if !c.held {
		return false
	}
	if !slices.Contains(c.state.Refused, step) {
		c.state.Refused = append(c.state.Refused, step)
		slices.Sort(c.state.Refused)
	}
	if antigravityWithholdsRepliesAfterRefusal {
		if turn := antigravityRecordedTurn(c.e, c.in.id()); turn != "" {
			c.state.Withheld = &antigravityWithheld{Turn: turn, From: step}
		}
	}
	return true
}

// refused reports whether step's content was refused.
func (c *antigravityConversation) refused(step int64) bool {
	_, found := slices.BinarySearch(c.state.Refused, step)
	return found
}

// repliesWithheld reports whether a reply at step of turn is withheld after a refusal.
func (c *antigravityConversation) repliesWithheld(turn string, step int64) bool {
	w := c.state.Withheld
	return antigravityWithholdsRepliesAfterRefusal && w != nil && w.Turn == turn && step > w.From
}

// endTurn clears what a turn's refusals left, at its Stop.
func (c *antigravityConversation) endTurn() {
	c.state.Refused, c.state.Withheld = nil, nil
}

// close saves the state, when it changed or began, and lets the lock go.
func (c *antigravityConversation) close() {
	if !c.held {
		return
	}
	c.held = false
	defer c.unlock()
	// A state that began is saved even unchanged: a subagent's first hook runs before its
	// transcript exists, and the next must not begin again past the subagent's first call.
	if b, err := json.Marshal(c.state); err == nil && (c.began || !bytes.Equal(b, c.before)) {
		if err := hookrun.WriteState(c.path, b); err != nil {
			c.e.Logf("antigravity calls state: %v", err)
		}
	}
	if c.created {
		hookrun.PruneState(filepath.Dir(c.path), c.e.Time().Add(-spool.MaxAge))
	}
}

// antigravityRecordedTurn is the turn the conversation's record names, whatever checkout or
// lane wrote it; "" for none.
func antigravityRecordedTurn(e hookrun.Env, sessionID string) string {
	b := antigravityTurnBegun(e, sessionID)
	if b == nil || len(b) > 4<<10 {
		return ""
	}
	var turn antigravityTurn
	if json.Unmarshal(b, &turn) != nil || !hookrun.ShortLabel(turn.TurnID) {
		return ""
	}
	return turn.TurnID
}
