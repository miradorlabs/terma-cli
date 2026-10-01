package hookrun

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
)

// The core's tests drive sessions through Extension, the one handler set the core owns,
// under Claude Code's label. editFile takes the payload shape these tests were written
// in, tool_input.file_path, and hands the extension its own.
var testAgent = Extension{Tool: "claude-code"}

func startSession(ctx context.Context, env Env) error { return testAgent.sessionStart(ctx, env) }

func endSession(ctx context.Context, env Env) error { return testAgent.sessionEnd(ctx, env) }

func editFile(ctx context.Context, env Env) error {
	var in struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		Model     string `json:"model"`
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			FilePath string `json:"file_path"`
		} `json:"tool_input"`
	}
	data, _ := io.ReadAll(env.Stdin)
	_ = json.Unmarshal(data, &in)
	out, _ := json.Marshal(map[string]string{"session_id": in.SessionID, "cwd": in.Cwd, "model": in.Model, "file": in.ToolInput.FilePath, "tool": in.ToolName})
	env.Stdin = bytes.NewReader(out)
	return testAgent.fileEdit(ctx, env)
}
