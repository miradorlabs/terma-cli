package gemini

import (
	"cmp"
	"context"
	"encoding/json"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/reach"
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

// geminiReach is where a read's, a search's or a shell command's input names what it
// reached: read_file, read_many_files, list_directory, glob, search_file_content and
// run_shell_command, whose directory is relative to the cwd.
var geminiReach = reach.Spec{
	Paths:    []string{"file_path", "absolute_path", "path", "dir_path", "paths"},
	Commands: []string{"command"},
	Dirs:     []string{"directory", "dir_path"},
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
	env.StartRun(in.SessionID)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.Announce(r, env.NewSession(r, in.SessionID, geminiTool, ""), nil)
	return nil
}

// prompt is BeforeAgent: the caller claims the session from the payload; nothing is recorded.
func prompt(_ context.Context, env hookrun.Env) error {
	_, _ = readGeminiInput(env)
	return nil
}

// afterTool records what an edit tool wrote. Any other call, a read, a search or a shell
// command, only classifies what it reached (hookrun.Env.Classify), on a migrated machine
// alone: AfterTool matches every tool, and terma writes no BeforeTool, so a crossing is
// marked once the call has run.
func afterTool(ctx context.Context, env hookrun.Env) error {
	in, ok := readGeminiInput(env)
	if !ok {
		return nil
	}
	if !geminiEditTools[in.ToolName] {
		if env.Routing != nil {
			env.Cwd = cmp.Or(in.Cwd, env.Cwd)
			env.Classify(ctx, nil, session.Session{ID: in.SessionID, Tool: geminiTool}, geminiReach.Of(in.ToolInput, env.Cwd), nil)
		}
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
