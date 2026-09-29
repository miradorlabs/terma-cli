package hookrun

import (
	"cmp"
	"context"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// --- Pi adapter ----------------------------------------------------------------------

// piHookInput is the JSON terma's Pi extension (internal/harness/pi/terma.ts) writes to
// stdin, composed from Pi's own events. The session id is Pi's own
// (ctx.sessionManager.getSessionId()) — the session.id the extension's telemetry carries,
// so the claim it makes names the session the relay sees.
type piHookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	File      string `json:"file"`
	Tool      string `json:"tool"`
}

const piTool = "pi"

func readPiInput(r io.Reader) (*piHookInput, error) {
	in, err := readHookInput[piHookInput](r)
	if err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook input has no session_id")
	}
	return in, nil
}

// PiSessionStart records a Pi session as active.
func PiSessionStart(ctx context.Context, env Env) error {
	in, err := readPiInput(env.Stdin)
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
	attrs := map[string]any{attrSource: "session_start"}
	env.announce(r, env.newSession(r, in.SessionID, piTool, in.Model), attrs)
	return nil
}

// PiSessionEnd clears the active session; manifests stay for the commit to come.
func PiSessionEnd(ctx context.Context, env Env) error {
	in, err := readPiInput(env.Stdin)
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
	env.endSession(r, in.SessionID, piTool, "")
	return nil
}

// PiFileEdit adds one edited file to the session's manifest.
func PiFileEdit(ctx context.Context, env Env) error {
	in, err := readPiInput(env.Stdin)
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
	env.touch(r, session.Session{ID: in.SessionID, Tool: piTool, Model: in.Model}, in.Tool,
		relativeFiles(r, env.Cwd, []string{in.File}), nil)
	return nil
}

// PiPrompt is the extension's turn-start call. terma records nothing for it: the caller
// claims the session for the local relay from the payload and starts the relay, so a
// session whose start the extension missed, or a relay that died between turns, is
// covered before the turn exports (as UserPromptSubmit is for Claude Code).
func PiPrompt(_ context.Context, env Env) error {
	_, err := readPiInput(env.Stdin)
	return err
}
