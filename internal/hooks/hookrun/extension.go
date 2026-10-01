package hookrun

import (
	"cmp"
	"context"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// Extension is the hooks a terma-written agent extension calls, in one JSON shape, under
// the session id its telemetry carries too.
type Extension struct {
	Tool string
	// Source is the session start's source attribute, "session_start" when empty.
	Source string
}

// Events is the extension's <prefix>-session-start, -prompt, -session-end and -file-edit handlers.
func (x Extension) Events(prefix string) map[string]func(context.Context, Env) error {
	return map[string]func(context.Context, Env) error{
		prefix + "-session-start": x.sessionStart,
		prefix + "-prompt":        TurnStart,
		prefix + "-session-end":   x.sessionEnd,
		prefix + "-file-edit":     x.fileEdit,
	}
}

type extensionHookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	File      string `json:"file"`
	Tool      string `json:"tool"`
	Reason    string `json:"reason"`
	// ParentSessionID is set on a session an agent opened for a subagent.
	ParentSessionID string `json:"parent_session_id"`
}

func readExtensionInput(r io.Reader) (*extensionHookInput, error) {
	in, err := ReadInput[extensionHookInput](r)
	if err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook input has no session_id")
	}
	return in, nil
}

func (x Extension) sessionStart(ctx context.Context, env Env) error {
	in, err := readExtensionInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	attrs := map[string]any{AttrSource: cmp.Or(x.Source, "session_start")}
	sess := env.NewSession(r, in.SessionID, x.Tool, in.Model)
	if !session.ValidID(in.ParentSessionID) {
		env.Announce(r, sess, attrs)
		return nil
	}
	// A subagent's session is announced, never made active: it would claim the
	// developer's next hand-written commit. Its edits still build its own manifest.
	attrs[AttrParentSession] = in.ParentSessionID
	env.PruneManifests(r, sess.UpdatedAt)
	env.EmitStart(r, sess, attrs)
	return nil
}

func (x Extension) sessionEnd(ctx context.Context, env Env) error {
	in, err := readExtensionInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	env.EndSession(r, in.SessionID, x.Tool, in.Reason)
	return nil
}

func (x Extension) fileEdit(ctx context.Context, env Env) error {
	in, err := readExtensionInput(env.Stdin)
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
	env.Touch(r, session.Session{ID: in.SessionID, Tool: x.Tool, Model: in.Model}, in.Tool,
		RelativeFiles(r, env.Cwd, []string{in.File}), nil)
	return nil
}

// TurnStart records nothing: its caller claims the session and starts the relay before the
// turn exports. It must print nothing, since stdout may reach the model.
func TurnStart(_ context.Context, env Env) error {
	_, err := readExtensionInput(env.Stdin)
	return err
}
