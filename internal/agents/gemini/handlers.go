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

// prompt is BeforeAgent: the caller claims the session and records the turn start for post-tool reach cutoffs.
func prompt(_ context.Context, env hookrun.Env) error {
	if in, ok := readGeminiInput(env); ok {
		env.StartTurn(in.SessionID)
	}
	return nil
}

func afterTool(ctx context.Context, env hookrun.Env) error {
	in, ok := readGeminiInput(env)
	if !ok {
		return nil
	}
	env.MixedSince = env.TurnSince(in.SessionID)
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	sess := session.Session{ID: in.SessionID, Tool: geminiTool}
	if !geminiEditTools[in.ToolName] {
		if env.Routing != nil {
			env.Classify(ctx, nil, sess, reach.Gemini.Of(in.ToolInput, env.Cwd), nil)
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
	if r, err := env.Repo(ctx); err != nil {
		env.Classify(ctx, nil, sess, []string{file}, nil)
	} else {
		env.Touch(r, sess, in.ToolName, []string{file}, nil)
	}
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
