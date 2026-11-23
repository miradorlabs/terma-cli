package hookrun

import (
	"cmp"
	"context"
	"errors"
	"io"
	"maps"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Extension is the hooks a terma-written agent extension calls, in one JSON shape, under
// the session id its telemetry carries too.
type Extension struct {
	Tool string
	// ClaimParent names a subagent's parent in its relay claim, for the relay to stamp on
	// the subagent's telemetry: for an agent whose own telemetry does not name the parent.
	ClaimParent bool
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
	// Files are the files one tool call changed, where it changed several.
	Files []string `json:"files"`
	Tool  string   `json:"tool"`
	// ToolCallID is the call's id, as the agent's own telemetry names it.
	ToolCallID string `json:"tool_call_id"`
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
	sess := env.NewSession(r, in.SessionID, x.Tool, in.Model)
	if !session.ValidID(in.ParentSessionID) {
		env.Announce(r, sess, nil)
		return nil
	}
	env.StartSubagent(r, sess, in.ParentSessionID, x.ClaimParent, nil)
	return nil
}

// StartSubagent announces sess, a session an agent opened for a subagent of the session
// parent: it ages out old manifests and spools the start naming parent
// (terma.session.parent.id), with extra attributes only one agent knows. The session is never
// made active: it would claim the developer's next hand-written commit. Its edits still build
// its own manifest. With claimParent, the session's relay claim names parent too, for the
// relay to stamp on the subagent's telemetry, where the agent's own does not name it.
func (e Env) StartSubagent(r *Repo, sess session.Session, parent string, claimParent bool, extra map[string]any) {
	e.PruneManifests(r, sess.UpdatedAt)
	if claimParent {
		e.parent = parent
	}
	attrs := map[string]any{semconv.TermaSessionParentIDKey: parent}
	maps.Copy(attrs, extra)
	e.EmitStart(r, sess, attrs)
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
	env.EndSession(r, in.SessionID, x.Tool)
	return nil
}

func (x Extension) fileEdit(ctx context.Context, env Env) error {
	in, err := readExtensionInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	paths := slices.DeleteFunc(append([]string{in.File}, in.Files...), func(p string) bool { return p == "" })
	if !session.ValidID(in.SessionID) || len(paths) == 0 {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	attrs := map[string]any{}
	BoundedAttr(attrs, semconv.GenAIToolCallIDKey, in.ToolCallID)
	env.Touch(r, session.Session{ID: in.SessionID, Tool: x.Tool, Model: in.Model}, in.Tool, paths, attrs)
	return nil
}

// TurnStart records nothing: its caller claims the session and starts the relay before the
// turn exports. It must print nothing, since stdout may reach the model.
func TurnStart(_ context.Context, env Env) error {
	_, err := readExtensionInput(env.Stdin)
	return err
}
