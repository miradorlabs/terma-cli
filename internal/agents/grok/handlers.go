package grok

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// grokHookInput is the part of a grok hook's stdin terma reads. grok sends every field twice,
// in its own camelCase and in Claude Code's snake_case, except promptId, subagentId and
// subagentType, which it sends in camelCase only (grok 1.0.92).
type grokHookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Source    string `json:"source"`
	Prompt    string `json:"prompt"`
	PromptID  string `json:"promptId"`
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
	// ToolResponse is the tool's result; only its type and whether it applied an edit are read.
	ToolResponse json.RawMessage `json:"tool_response"`
	// SubagentID is the child's session, on SubagentStart, where session_id is the parent.
	SubagentID string `json:"subagentId"`
	// SubagentType names the subagent's kind: on SubagentStart, and on every hook of a child.
	SubagentType string `json:"subagentType"`
}

// child reports whether the payload is a subagent's own hook: grok names the kind on every
// hook of a child, and never makes one the active session.
func (in *grokHookInput) child() bool { return in.SubagentType != "" }

func readInput(r io.Reader) (*grokHookInput, error) {
	in, err := hookrun.ReadInput[grokHookInput](r)
	if err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook input has no session_id")
	}
	return in, nil
}

// sessionStart announces the session, claims it and makes it active. grok fires none for a
// subagent, whose start is SubagentStart's; one that names a kind is still never made active.
func sessionStart(ctx context.Context, env hookrun.Env) error {
	in, err := readInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	sess := env.NewSession(r, in.SessionID, name, "")
	if in.child() {
		env.PruneManifests(r, sess.UpdatedAt)
		env.EmitStart(r, sess, nil)
		return nil
	}
	env.Announce(r, sess, nil)
	return nil
}

// sessionEnd spools the end and clears the active session if it is this one. A subagent's end
// clears nothing: its session was never active, and its parent's stays so.
func sessionEnd(ctx context.Context, env hookrun.Env) error {
	in, err := readInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	env.EndSession(r, in.SessionID, name)
	return nil
}

// stop records nothing: dispatch claims the session from the payload and starts the flush.
func stop(_ context.Context, env hookrun.Env) error {
	_, err := readInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
	}
	return nil
}

// postToolUse records an edit grok's tool applied, with its call id, which the export's
// tool_use_id joins: a touch names its checkout, and the platform reads that as proof the file
// changed. So only a result that proves the edit sends one, search_replace's
// {"type":"SearchReplace","EditsApplied":{…}}; any other result, a write's included, whose
// shape is not known, sends none. A subagent's touch names its agent; the manifest stays the
// session's.
func postToolUse(ctx context.Context, env hookrun.Env) error {
	in, err := readInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) || in.ToolInput.FilePath == "" || !editsApplied(in.ToolResponse) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	attrs := map[string]any{}
	if in.child() {
		attrs = hookrun.AgentAttrs(attrs, in.SessionID, in.SubagentType)
	}
	hookrun.BoundedAttr(attrs, semconv.GenAIToolCallIDKey, in.ToolUseID)
	env.Touch(r, session.Session{ID: in.SessionID, Tool: name}, in.ToolName, []string{in.ToolInput.FilePath}, attrs)
	return nil
}

// editsApplied reports whether a tool's result is search_replace's applied edit.
func editsApplied(raw json.RawMessage) bool {
	var result struct {
		Type         string          `json:"type"`
		EditsApplied json.RawMessage `json:"EditsApplied"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Type != "SearchReplace" {
		return false
	}
	applied := bytes.TrimSpace(result.EditsApplied)
	return len(applied) > 0 && applied[0] == '{'
}
