package gemini

import (
	"cmp"
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// geminiHookInput is a hook's stdin; session_id is the session.id its telemetry carries.
type geminiHookInput struct {
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

const geminiTool = "gemini"

var geminiEditTools = map[string]bool{"write_file": true, "replace": true, "edit": true}

func readGeminiInput(env hookrun.Env) (*geminiHookInput, bool) {
	in, err := hookrun.ReadInput[geminiHookInput](env.Stdin)
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

func sessionStart(ctx context.Context, env hookrun.Env) error {
	in, ok := readGeminiInput(env)
	if !ok {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.Announce(r, env.NewSession(r, in.SessionID, geminiTool, ""), nil)
	return nil
}

// prompt is BeforeAgent: reading the payload lets dispatch claim the session from it.
func prompt(_ context.Context, env hookrun.Env) error {
	_, _ = readGeminiInput(env)
	return nil
}

func afterTool(ctx context.Context, env hookrun.Env) error {
	in, ok := readGeminiInput(env)
	if !ok {
		return nil
	}
	if !geminiEditTools[in.ToolName] {
		return nil
	}
	var input struct {
		FilePath     string `json:"file_path"`
		Path         string `json:"path"`
		AbsolutePath string `json:"absolute_path"`
	}
	_ = json.Unmarshal(in.ToolInput, &input)
	file := cmp.Or(input.FilePath, input.AbsolutePath, input.Path)
	if file == "" {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.Touch(r, session.Session{ID: in.SessionID, Tool: geminiTool}, in.ToolName, []string{file}, nil)
	return nil
}

// sessionEnd clears the active session; manifests stay for the commit to come.
func sessionEnd(ctx context.Context, env hookrun.Env) error {
	in, ok := readGeminiInput(env)
	if !ok {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.EndSession(r, in.SessionID, geminiTool)
	return nil
}
