package hookrun

import (
	"cmp"
	"context"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// Extension is the hooks of an agent that has none terma can commit and no exporter of
// its own: terma writes an extension into it that exports its telemetry itself and calls
// `terma hook <prefix>-*` with one JSON shape, composed from the agent's own events. The
// session id is the agent's own, the session.id its extension stamps on every record, so
// a claim names the session the relay sees. Only the tool label differs between agents.
type Extension struct {
	// Tool is the agent's label.
	Tool string
}

// Events is the extension's events: <prefix>-session-start, -prompt, -session-end and
// -file-edit.
func (x Extension) Events(prefix string) map[string]func(context.Context, Env) error {
	return map[string]func(context.Context, Env) error{
		prefix + "-session-start": x.sessionStart,
		prefix + "-prompt":        TurnStart,
		prefix + "-session-end":   x.sessionEnd,
		prefix + "-file-edit":     x.fileEdit,
	}
}

// extensionHookInput is what an extension writes to stdin.
type extensionHookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	File      string `json:"file"`
	Tool      string `json:"tool"`
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

// sessionStart records an extension-driven session as active.
func (x Extension) sessionStart(ctx context.Context, env Env) error {
	in, err := readExtensionInput(env.Stdin)
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
	env.Announce(r, env.NewSession(r, in.SessionID, x.Tool, in.Model), map[string]any{AttrSource: "session_start"})
	return nil
}

// sessionEnd clears the active session; manifests stay for the commit to come.
func (x Extension) sessionEnd(ctx context.Context, env Env) error {
	in, err := readExtensionInput(env.Stdin)
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
	env.EndSession(r, in.SessionID, x.Tool, "")
	return nil
}

// fileEdit adds one edited file to the session's manifest.
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

// TurnStart is a turn-start hook that records nothing: the caller claims the session
// for the local relay from the payload and starts the relay, so a session whose start
// was missed, or a relay that died between turns, is covered before the turn exports.
// It must print nothing: an agent may hand the hook's stdout to the model.
func TurnStart(_ context.Context, env Env) error {
	_, err := readExtensionInput(env.Stdin)
	return err
}
