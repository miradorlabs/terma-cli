package copilot

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/applypatch"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// hookInput is a hook's stdin; session_id is the gen_ai.conversation.id its telemetry carries.
type hookInput struct {
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	// ToolResult is the Copilot runtime's, in the CLI and in VS Code's agent host.
	ToolResult struct {
		ResultType string `json:"result_type"`
	} `json:"tool_result"`
}

// editTools are the tools that write the files they name. The Copilot runtime calls its
// own Edit and Write (seen with CLI 1.0.96, alone and inside VS Code 1.141); the rest are
// the edit tools of VS Code's own agent, by the names its extension gives them.
var editTools = map[string]bool{
	"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true,
	"apply_patch": true, "create_file": true, "replace_string_in_file": true, "multi_replace_string_in_file": true,
	"insert_edit_into_file": true, "edit_notebook_file": true,
}

func readInput(env hookrun.Env) (*hookInput, bool) {
	in, err := hookrun.ReadInput[hookInput](env.Stdin)
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
	in, ok := readInput(env)
	if !ok {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.Announce(r, env.NewSession(r, in.SessionID, name, ""), nil)
	return nil
}

// prompt is UserPromptSubmit, which the CLI fires before SessionStart: the caller claims
// the session from the payload; nothing is recorded.
func prompt(_ context.Context, env hookrun.Env) error {
	_, _ = readInput(env)
	return nil
}

func postToolUse(ctx context.Context, env hookrun.Env) error {
	in, ok := readInput(env)
	// A result the runtime names as anything but a success wrote nothing.
	if !ok || !editTools[in.ToolName] || in.ToolResult.ResultType != "" && in.ToolResult.ResultType != "success" {
		return nil
	}
	paths := editedPaths(in.ToolInput)
	if len(paths) == 0 {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.Touch(r, session.Session{ID: in.SessionID, Tool: name}, in.ToolName, paths, nil)
	return nil
}

// editedPaths are the files an edit tool's input names: a path under whichever key the
// tool spells it, one per replacement of a multi-file edit, or the files of an apply_patch
// envelope, which is the whole input or its "input".
func editedPaths(raw json.RawMessage) []string {
	var patch string
	if json.Unmarshal(raw, &patch) == nil {
		return applypatch.Paths(patch)
	}
	var input struct {
		Path         string `json:"path"`
		FilePath     string `json:"file_path"`
		FilePathVS   string `json:"filePath"`
		NotebookPath string `json:"notebook_path"`
		Input        string `json:"input"`
		Replacements []struct {
			FilePath string `json:"filePath"`
		} `json:"replacements"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return nil
	}
	var out []string
	add := func(p string) {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	add(cmp.Or(input.Path, input.FilePath, input.FilePathVS, input.NotebookPath))
	for _, r := range input.Replacements {
		add(r.FilePath)
	}
	for _, p := range applypatch.Paths(input.Input) {
		add(p)
	}
	return out
}

// stop is the end of a turn: the caller claims the session and flushes the spool; the
// session's name is recorded when new or renamed.
func stop(ctx context.Context, env hookrun.Env) error {
	in, ok := readInput(env)
	if !ok {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	captureTitle(env, r, in.SessionID)
	return nil
}

// sessionEnd clears the active session; manifests stay for the commit to come.
func sessionEnd(ctx context.Context, env hookrun.Env) error {
	in, ok := readInput(env)
	if !ok {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	captureTitle(env, r, in.SessionID)
	env.EndSession(r, in.SessionID, name)
	return nil
}
