package hookrun

import (
	"cmp"
	"context"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// --- OpenCode adapter ----------------------------------------------------------------

// opencodeHookInput is the JSON Terma's OpenCode plugin writes to stdin. The plugin
// composes it from OpenCode's own events, so only what attribution needs is here.
type opencodeHookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	Reason    string `json:"reason"`
	File      string `json:"file"`
	Tool      string `json:"tool"`
	// ParentSessionID is OpenCode's Session.parentID: set on a session its task tool
	// opened for a subagent, absent on one a person started.
	ParentSessionID string `json:"parent_session_id"`
}

const opencodeTool = "opencode"

func readOpenCodeInput(r io.Reader) (*opencodeHookInput, error) {
	in, err := readHookInput[opencodeHookInput](r)
	if err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook input has no session_id")
	}
	return in, nil
}

// OpenCodeSessionStart records an OpenCode session as active.
func OpenCodeSessionStart(ctx context.Context, env Env) error {
	in, err := readOpenCodeInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		env.logf("ignoring unsafe session id")
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		env.logf("not in a git repository: %v", err)
		return nil
	}
	attrs := map[string]any{attrSource: "session.created"}
	sess := env.newSession(r, in.SessionID, opencodeTool, in.Model)
	if !session.ValidID(in.ParentSessionID) {
		env.announce(r, sess, attrs)
		return nil
	}
	// A session the task tool opened for a subagent is announced, never made active. The
	// active session is what claims a commit no manifest accounts for, and that belongs
	// to the session a person is driving: a child that displaced it would put its own id
	// on the developer's next hand-written commit. The child's edits still build a
	// manifest of their own, and that is how they are attributed.
	attrs[attrParentSession] = in.ParentSessionID
	env.pruneManifests(r, sess.UpdatedAt)
	env.emitStart(r, sess, attrs)
	return nil
}

// OpenCodeSessionEnd clears the active session; manifests stay for the commit to come.
func OpenCodeSessionEnd(ctx context.Context, env Env) error {
	in, err := readOpenCodeInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		env.logf("ignoring unsafe session id")
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	env.endSession(r, in.SessionID, opencodeTool, in.Reason)
	return nil
}

// OpenCodeFileEdit adds one edited file to the session's manifest.
func OpenCodeFileEdit(ctx context.Context, env Env) error {
	in, err := readOpenCodeInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) || in.File == "" {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	env.touch(r, session.Session{ID: in.SessionID, Tool: opencodeTool, Model: in.Model}, in.Tool,
		relativeFiles(r, env.Cwd, []string{in.File}), nil)
	return nil
}
