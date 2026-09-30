package opencode

import (
	"cmp"
	"context"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/hookrun"
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
	in, err := hookrun.ReadInput[opencodeHookInput](r)
	if err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook input has no session_id")
	}
	return in, nil
}

// sessionStart records an OpenCode session as active.
func sessionStart(ctx context.Context, env hookrun.Env) error {
	in, err := readOpenCodeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		env.Logf("ignoring unsafe session id")
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		env.Logf("not in a git repository: %v", err)
		return nil
	}
	attrs := map[string]any{hookrun.AttrSource: "session.created"}
	sess := env.NewSession(r, in.SessionID, opencodeTool, in.Model)
	if !session.ValidID(in.ParentSessionID) {
		env.Announce(r, sess, attrs)
		return nil
	}
	// A session the task tool opened for a subagent is announced, never made active. The
	// active session is what claims a commit no manifest accounts for, and that belongs
	// to the session a person is driving: a child that displaced it would put its own id
	// on the developer's next hand-written commit. The child's edits still build a
	// manifest of their own, and that is how they are attributed.
	attrs[hookrun.AttrParentSession] = in.ParentSessionID
	env.PruneManifests(r, sess.UpdatedAt)
	env.EmitStart(r, sess, attrs)
	return nil
}

// sessionEnd clears the active session; manifests stay for the commit to come.
func sessionEnd(ctx context.Context, env hookrun.Env) error {
	in, err := readOpenCodeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		env.Logf("ignoring unsafe session id")
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.EndSession(r, in.SessionID, opencodeTool, in.Reason)
	return nil
}

// fileEdit adds one edited file to the session's manifest.
func fileEdit(ctx context.Context, env hookrun.Env) error {
	in, err := readOpenCodeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) || in.File == "" {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.Touch(r, session.Session{ID: in.SessionID, Tool: opencodeTool, Model: in.Model}, in.Tool,
		hookrun.RelativeFiles(r, env.Cwd, []string{in.File}), nil)
	return nil
}
