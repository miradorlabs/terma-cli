package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// antigravityConversation is one hook's hold of a conversation's calls state, under its lock,
// taken once per hook (the package comment says where it fits): how far the transcript was
// read, the batch being read, and the clock every event of the conversation is stamped from.
// A hook that cannot have the lock sends what it sends without it, at the real time, and no
// content.
type antigravityConversation struct {
	// Fixed for the hook: its environment, the checkout it ran in, and its payload.
	e  hookrun.Env
	r  *hookrun.Repo
	in *antigravityHookInput

	// The conversation's saved state, read and written under its lock. held is that the lock
	// is held and state is the conversation's; path is the state's file.
	held   bool
	unlock func()
	path   string
	state  antigravityCallState
	// began is a state that begins (see turn.go); before is the state as loaded, to save it
	// only when it changed; created is that there was no state file.
	began   bool
	before  []byte
	created bool

	// This hook's scratch, gone with it.
	// content is the content the hook may attach, worked out once (contentFor).
	content *antigravityContent
	// paths judges the paths the hook meets, each folder placed once.
	paths *antigravityPaths
	// call is the tool call a PostToolUse spools, and its time.
	call antigravityOwnCall
}

// antigravityOwnCall is the tool call a PostToolUse spools, and its time. The hook reads the
// transcript before it spools the call, and the read may meet the call's own result, the
// GENERIC line at the call's step: the call's time is then stamped just before that result's,
// so a result never sorts before its call. The model call that asked for the tool sorts
// before both.
type antigravityOwnCall struct {
	// step is the call's step, -1 for no call.
	step int64
	// at is the call's time once stamped, zero before.
	at time.Time
}

// meet is the read reaching a GENERIC line at step: the call's own result, the first time,
// stamps the call's time with stamp.
func (o *antigravityOwnCall) meet(step int64, stamp func() time.Time) {
	if step == o.step && o.at.IsZero() {
		o.at = stamp()
	}
}

// time is the call's time: the one stamped before its result when the read met that, else
// next's.
func (o *antigravityOwnCall) time(next func() time.Time) time.Time {
	if !o.at.IsZero() {
		return o.at
	}
	return next()
}

// openAntigravityConversation takes the conversation's lock, waiting up to
// antigravityCallLockWait for another of its hooks, and loads its calls state. paths judges
// the paths the hook meets under it, with what the hook placed before it took the lock; nil
// for a hook that placed none. Without a transcript of agy's shape, a spool or a safe id, it
// holds nothing.
func openAntigravityConversation(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *antigravityHookInput, paths *antigravityPaths) *antigravityConversation {
	if paths == nil {
		paths = newAntigravityPaths(e.Policy)
	}
	c := &antigravityConversation{e: e, r: r, in: in, call: antigravityOwnCall{step: -1}, paths: paths}
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
	// no taint or clock of an older one.
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

// callTime is the time of the call this PostToolUse spools (antigravityOwnCall).
func (c *antigravityConversation) callTime() time.Time { return c.call.time(c.now) }

// contentFor is the content this hook may attach, worked out once.
func (c *antigravityConversation) contentFor() antigravityContent {
	if c.content == nil {
		content := antigravityContentFor(c.e, c.in, c.paths)
		c.content = &content
	}
	return *c.content
}

// refuses reports whether the content of a tool call with these arguments is refused
// (antigravityPaths.refuses), a relative path judged from the conversation's cwd: the
// transcript's reader judges every call a call line asks for by it. A PostToolUse judges its
// own call by the same paths before it takes the lock (postToolUse).
func (c *antigravityConversation) refuses(args json.RawMessage) bool {
	return c.paths.refuses(c.e.Cwd, args)
}

// close saves the state, when it changed or began, and lets the lock go. A hook defers it
// once, as it opens the conversation.
func (c *antigravityConversation) close() {
	if !c.held {
		return
	}
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
