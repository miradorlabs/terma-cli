package hookrun

import (
	"cmp"
	"context"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// --- extension-driven agents -----------------------------------------------------------

// Pi and Hermes have no hooks terma can commit and no exporter of their own: terma
// writes an extension into each (internal/harness/pi/terma.ts, internal/harness/hermes)
// that exports their telemetry itself and calls `terma hook <agent>-*` with one JSON
// shape, composed from the agent's own events. The session id is the agent's own —
// the session.id its extension stamps on every record — so a claim names the session
// the relay sees. The handlers are the same for every such agent; only the tool label
// differs.

// extensionHookInput is what an extension writes to stdin.
type extensionHookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	File      string `json:"file"`
	Tool      string `json:"tool"`
}

const (
	piTool     = "pi"
	hermesTool = "hermes"
)

func readExtensionInput(r io.Reader) (*extensionHookInput, error) {
	in, err := readHookInput[extensionHookInput](r)
	if err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook input has no session_id")
	}
	return in, nil
}

// extensionSessionStart records an extension-driven session as active.
func extensionSessionStart(tool string) func(context.Context, Env) error {
	return func(ctx context.Context, env Env) error {
		in, err := readExtensionInput(env.Stdin)
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
		env.announce(r, env.newSession(r, in.SessionID, tool, in.Model), map[string]any{attrSource: "session_start"})
		return nil
	}
}

// extensionSessionEnd clears the active session; manifests stay for the commit to come.
func extensionSessionEnd(tool string) func(context.Context, Env) error {
	return func(ctx context.Context, env Env) error {
		in, err := readExtensionInput(env.Stdin)
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
		env.endSession(r, in.SessionID, tool, "")
		return nil
	}
}

// extensionFileEdit adds one edited file to the session's manifest.
func extensionFileEdit(tool string) func(context.Context, Env) error {
	return func(ctx context.Context, env Env) error {
		in, err := readExtensionInput(env.Stdin)
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
		env.touch(r, session.Session{ID: in.SessionID, Tool: tool, Model: in.Model}, in.Tool,
			relativeFiles(r, env.Cwd, []string{in.File}), nil)
		return nil
	}
}

// extensionPrompt is an extension's turn-start call. terma records nothing for it: the
// caller claims the session for the local relay from the payload and starts the relay,
// so a session whose start the extension missed, or a relay that died between turns, is
// covered before the turn exports (as UserPromptSubmit is for Claude Code).
func extensionPrompt(_ context.Context, env Env) error {
	_, err := readExtensionInput(env.Stdin)
	return err
}

// Pi's handlers (`terma hook pi-*`).
var (
	PiSessionStart = extensionSessionStart(piTool)
	PiSessionEnd   = extensionSessionEnd(piTool)
	PiFileEdit     = extensionFileEdit(piTool)
	PiPrompt       = extensionPrompt
)

// OmpPrompt is the claim-only turn start of terma's omp relay extension; omp's
// committed hook file reports the rest (hookrun/omp.go).
var OmpPrompt = extensionPrompt

// Hermes's handlers (`terma hook hermes-*`).
var (
	HermesSessionStart = extensionSessionStart(hermesTool)
	HermesSessionEnd   = extensionSessionEnd(hermesTool)
	HermesFileEdit     = extensionFileEdit(hermesTool)
	HermesPrompt       = extensionPrompt
)
