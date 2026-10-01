package omp

import (
	"cmp"
	"context"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// ompHookInput is what terma's hook extension writes to stdin, composed from omp's events.
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

func sessionStart(ctx context.Context, env hookrun.Env) error {
	in, err := readOmpInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
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
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	env.EndSession(r, in.SessionID, ompTool, "")
	return nil
}

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
