package hookrun

import (
	"cmp"
	"context"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// --- Gemini CLI --------------------------------------------------------------------------

// geminiHookInput is what Gemini CLI hands a hook on stdin (0.62): the session is
// Gemini's own session_id — the session.id its telemetry carries — and cwd the
// session's workspace. AfterTool adds the tool and its input.
type geminiHookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		FilePath     string `json:"file_path"`
		Path         string `json:"path"`
		AbsolutePath string `json:"absolute_path"`
	} `json:"tool_input"`
}

const geminiTool = "gemini"

// geminiEditTools are Gemini's tools that change a file.
var geminiEditTools = map[string]bool{"write_file": true, "replace": true, "edit": true}

func readGeminiInput(env Env) (*geminiHookInput, bool) {
	in, err := readHookInput[geminiHookInput](env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil, false
	}
	if !session.ValidID(in.SessionID) {
		env.Logf("ignoring a missing or unsafe session id")
		return nil, false
	}
	return in, true
}

// GeminiSessionStart records a Gemini session as active.
func GeminiSessionStart(ctx context.Context, env Env) error {
	in, ok := readGeminiInput(env)
	if !ok {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.Announce(r, env.NewSession(r, in.SessionID, geminiTool, ""), map[string]any{attrSource: "session_start"})
	return nil
}

// GeminiPrompt is Gemini's BeforeAgent, at every turn: the caller claims the session
// from the payload and starts the relay; nothing is recorded.
func GeminiPrompt(_ context.Context, env Env) error {
	_, _ = readGeminiInput(env)
	return nil
}

// GeminiAfterTool adds the file an editing tool changed to the session's manifest.
func GeminiAfterTool(ctx context.Context, env Env) error {
	in, ok := readGeminiInput(env)
	if !ok || !geminiEditTools[in.ToolName] {
		return nil
	}
	file := cmp.Or(in.ToolInput.FilePath, in.ToolInput.AbsolutePath, in.ToolInput.Path)
	if file == "" {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.Touch(r, session.Session{ID: in.SessionID, Tool: geminiTool}, in.ToolName, RelativeFiles(r, env.Cwd, []string{file}), nil)
	return nil
}

// GeminiSessionEnd clears the active session; manifests stay for the commit to come.
func GeminiSessionEnd(ctx context.Context, env Env) error {
	in, ok := readGeminiInput(env)
	if !ok {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.EndSession(r, in.SessionID, geminiTool, "")
	return nil
}
