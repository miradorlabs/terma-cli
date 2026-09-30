package omp

import (
	"cmp"
	"context"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// --- omp adapter ----------------------------------------------------------------------

// ompHookInput is the JSON Terma's omp hook extension writes to stdin. The extension
// composes it from omp's own events, so only what attribution needs is here. The
// session id is the extension's own: omp fires session_start without one, so the
// extension mints a UUID per process and reuses it for the session's whole life.
type ompHookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	File      string `json:"file"`
	Tool      string `json:"tool"`
}

const ompTool = "omp"

func readOmpInput(r io.Reader) (*ompHookInput, error) {
	in, err := hookrun.ReadInput[ompHookInput](r)
	if err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook input has no session_id")
	}
	return in, nil
}

// sessionStart records an omp session as active.
func sessionStart(ctx context.Context, env hookrun.Env) error {
	in, err := readOmpInput(env.Stdin)
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
	attrs := map[string]any{hookrun.AttrSource: "session_start"}
	env.Announce(r, env.NewSession(r, in.SessionID, ompTool, in.Model), attrs)
	return nil
}

// sessionEnd clears the active session; manifests stay for the commit to come.
func sessionEnd(ctx context.Context, env hookrun.Env) error {
	in, err := readOmpInput(env.Stdin)
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
	// The extension's id belongs to this session alone, so end it directly rather
	// than risk clearing an active session another tool started.
	env.EndSession(r, in.SessionID, ompTool, "")
	return nil
}

// fileEdit adds one edited file to the session's manifest.
func fileEdit(ctx context.Context, env hookrun.Env) error {
	in, err := readOmpInput(env.Stdin)
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
	env.Touch(r, session.Session{ID: in.SessionID, Tool: ompTool, Model: in.Model}, in.Tool,
		hookrun.RelativeFiles(r, env.Cwd, []string{in.File}), nil)
	return nil
}
