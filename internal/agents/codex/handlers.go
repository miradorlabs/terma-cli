package codex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// State directories under the hook state directory.
const (
	codexFundingCursorDir = "funding-cursors"
	codexReplyCursorDir   = "reply-cursors"
	codexDesktopCursorDir = "desktop-cursors"
	codexTitleStateDir    = "codex-titles"
	codexToolStartDir     = "codex-tool-starts"
)

// Values of evidence_source.
const (
	sourceCodexRollout      = "codex_rollout"
	sourceCodexHook         = "codex_hook"
	sourceCodexSessionIndex = "codex_session_index"
)

// codexCaptureTimeout bounds each rollout capture: both must fit inside Stop's
// three-second hook timeout.
const codexCaptureTimeout = time.Second

// codexNotify is the JSON Codex passes to its `notify` program at the end of each turn;
// it names no files, so it attributes only through the active-session fallback.
type codexNotify struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread-id"`
	TurnID   string `json:"turn-id"`
	Cwd      string `json:"cwd"`
	Model    string `json:"model"`
}

const codexTool = "codex"

// notifyHook marks the thread active and drains its rollout quota; being user-scope, it
// is the funding capture setup can guarantee in every repository.
func notifyHook(ctx context.Context, env hookrun.Env) error {
	if len(env.Args) == 0 {
		return nil
	}
	payload := env.Args[0]
	defer func() {
		if err := runPreviousNotify(ctx, payload); err != nil {
			env.Logf("previous Codex notify: %v", err)
		}
	}()
	var n codexNotify
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		env.Logf("parse codex notify: %v", err)
		return nil
	}
	id := cmp.Or(n.ThreadID, n.TurnID)
	if !session.ValidID(id) {
		return nil
	}
	env.Cwd = cmp.Or(n.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	// notify carries no transcript_path; the confined reader finds the rollout by thread id.
	turn := &codexHookInput{SessionID: id, Cwd: n.Cwd, Model: n.Model, TurnID: n.TurnID}
	captureCodexFunding(ctx, env, r, turn)
	captureCodexDesktopActivity(ctx, env, r, turn)
	captureCodexReplies(ctx, env, r, turn)
	captureCodexTitle(ctx, env, r, turn)
	// Not announce: notify fires every turn and does not age out manifests.
	sess := env.NewSession(r, id, codexTool, n.Model)
	env.SetActive(r, sess)
	env.EmitStart(r, sess, map[string]any{hookrun.AttrSource: n.Type})
	return nil
}

// codexHookInput is the subset of Codex's hook stdin schema terma reads. Its session_id is
// the same thread as notify's `thread-id`, so hooks and notify record one session.
type codexHookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Event          string `json:"hook_event_name"`
	Cwd            string `json:"cwd"`
	Model          string `json:"model"`
	Source         string `json:"source"`
	Reason         string `json:"reason"`
	TurnID         string `json:"turn_id"`
	AgentID        string `json:"agent_id"`
	AgentType      string `json:"agent_type"`
	ToolName       string `json:"tool_name"`
	PermissionMode string `json:"permission_mode"`
	Prompt         string `json:"prompt"`
	// ToolUseID matches Codex's OTLP call_id, so a file touch can join its tool call.
	ToolUseID string `json:"tool_use_id"`
	// ToolInput's `command` carries the apply_patch envelope that describes an edit.
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

const codexDesktopSurface = "desktop"

// codexDesktopRoute reports whether the routing record sends this repository's Codex
// Desktop logs; what content they carry is Env.Content's call.
func codexDesktopRoute(r *hookrun.Repo) bool {
	if r.ProjectID == "" {
		return false
	}
	rec, ok, err := r.Route()
	return err == nil && ok && slices.Contains(rec.Surfaces, name) &&
		slices.Contains(rec.Harnesses, name) && slices.Contains(rec.Signals, "logs")
}

func readCodexHookInput(r io.Reader) (*codexHookInput, error) {
	in, err := hookrun.ReadInput[codexHookInput](r)
	if err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook input has no session_id")
	}
	return in, nil
}

// sessionStart records the Codex session as active.
func sessionStart(ctx context.Context, env hookrun.Env) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	sess := env.NewSession(r, in.SessionID, codexTool, in.Model)
	env.SetActive(r, sess)
	env.PruneManifests(r, sess.UpdatedAt)
	attrs := map[string]any{hookrun.AttrSource: in.Source}
	if codexDesktopRoute(r) {
		attrs["capture_surface"] = codexDesktopSurface
		if dir, err := config.Dir(); err == nil {
			hookrun.PruneState(filepath.Join(dir, codexToolStartDir), env.Time().Add(-spool.MaxAge))
		}
	}
	// Codex fires no SessionStart for a spawned thread (it arrives as SubagentStart); this
	// covers a build that does.
	if spawn, status := rolloutSpawn(ctx, in.SessionID, in.TranscriptPath); status == hookrun.StatusPresent {
		codexSpawnAttrs(attrs, hookrun.AttrParentSession, spawn)
	}
	env.EmitStart(r, sess, attrs)
	return nil
}

// userPromptSubmit records a desktop turn, prompt and all: delivery withholds what the
// team's policy does not collect.
func userPromptSubmit(ctx context.Context, env hookrun.Env) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil || !session.ValidID(in.SessionID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	if !codexDesktopRoute(r) || in.TurnID == "" {
		return nil
	}
	attrs := hookrun.EvidenceAttrs(codexTool, sourceCodexHook, "UserPromptSubmit")
	attrs["capture_surface"] = codexDesktopSurface
	attrs["prompt_bytes"] = len(in.Prompt)
	hookrun.BoundedAttr(attrs, hookrun.AttrTurnID, in.TurnID)
	hookrun.BoundedAttr(attrs, hookrun.AttrModel, in.Model)
	attrs["prompt"] = boundedCodexContent(in.Prompt)
	env.EmitFor(r, spool.Event{Name: hookrun.EventUserPrompt, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
	return nil
}

// stop drains the thread's quota, replies and name before starting delivery.
func stop(ctx context.Context, env hookrun.Env) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	captureCodexFunding(ctx, env, r, in)
	captureCodexDesktopActivity(ctx, env, r, in)
	captureCodexReplies(ctx, env, r, in)
	captureCodexTitle(ctx, env, r, in)
	return nil
}

// sessionEnd clears the active session; manifests stay for the commit to come.
// Codex fires it late (on close, or half an hour idle) or never, so it only clears state
// and hookrun.ActiveTTL ages out the rest.
func sessionEnd(ctx context.Context, env hookrun.Env) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	// The end is spooled after the evidence.
	captureCodexFunding(ctx, env, r, in)
	captureCodexDesktopActivity(ctx, env, r, in)
	captureCodexReplies(ctx, env, r, in)
	captureCodexTitle(ctx, env, r, in)
	env.EndSession(r, in.SessionID, codexTool, in.Reason)
	return nil
}

// postToolUse records a Desktop tool call and adds the files its patch edited to the
// manifest; Codex has no file-edit event.
func postToolUse(ctx context.Context, env hookrun.Env) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	captureCodexFunding(ctx, env, r, in)
	if codexDesktopRoute(r) && in.ToolName != "" {
		attrs := hookrun.EvidenceAttrs(codexTool, sourceCodexHook, "PostToolUse")
		attrs["capture_surface"] = codexDesktopSurface
		hookrun.BoundedAttr(attrs, hookrun.AttrToolName, in.ToolName)
		hookrun.BoundedAttr(attrs, hookrun.AttrToolCallID, in.ToolUseID)
		hookrun.BoundedAttr(attrs, hookrun.AttrTurnID, in.TurnID)
		hookrun.BoundedAttr(attrs, hookrun.AttrModel, in.Model)
		if elapsed, ok := codexToolElapsed(env, in); ok {
			attrs["duration_ms"] = elapsed
			attrs["duration_source"] = "hook_elapsed"
		}
		attrs["arguments"] = boundedCodexContent(string(in.ToolInput))
		attrs["output"] = boundedCodexContent(string(in.ToolResponse))
		if success, known := codexToolSuccess(in.ToolResponse); known {
			if success {
				attrs[hookrun.AttrStatus] = "completed"
			} else {
				attrs[hookrun.AttrStatus] = "error"
			}
		}
		env.EmitFor(r, spool.Event{Name: hookrun.EventToolCall, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
	}
	captureCodexDesktopActivity(ctx, env, r, in)
	candidates := codexEditedPaths(in)
	if len(candidates) == 0 {
		return nil
	}
	// The call's path fields and its patch can name the same file.
	attrs := hookrun.AgentAttrs(map[string]any{}, in.AgentID, in.AgentType)
	hookrun.BoundedAttr(attrs, hookrun.AttrToolCallID, in.ToolUseID)
	if codexDesktopRoute(r) {
		attrs["capture_surface"] = codexDesktopSurface
	}
	env.Touch(r, session.Session{ID: in.SessionID, Tool: codexTool, Model: in.Model}, in.ToolName,
		hookrun.UniqueSorted(hookrun.RelativeFiles(r, env.Cwd, candidates)), attrs)
	return nil
}

func codexEditedPaths(in *codexHookInput) []string {
	var input struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	_ = json.Unmarshal(in.ToolInput, &input)
	var out []string
	for _, p := range []string{input.FilePath, input.Path} {
		if p != "" {
			out = append(out, p)
		}
	}
	return append(out, applyPatchPaths(input.Command)...)
}

const codexContentLimit = 16 << 10

func boundedCodexContent(s string) string {
	if len(s) > codexContentLimit {
		s = s[:codexContentLimit]
	}
	return strings.ToValidUTF8(s, "")
}

func codexToolSuccess(raw json.RawMessage) (bool, bool) {
	var result struct {
		IsError  *bool `json:"isError"`
		ExitCode *int  `json:"exit_code"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return false, false
	}
	if result.IsError != nil {
		return !*result.IsError, true
	}
	if result.ExitCode != nil {
		return *result.ExitCode == 0, true
	}
	return false, false
}

// applyPatchHeaders name a file; "Move to" is a rename's destination, recorded beside its
// source because the commit touches both.
var applyPatchHeaders = []string{
	"*** Add File:",
	"*** Update File:",
	"*** Delete File:",
	"*** Move to:",
}

// applyPatchPaths reads the envelope from the raw command text: Codex has no structured
// field, and the patch may be the tool's or heredoc'd into a shell call.
func applyPatchPaths(command string) []string {
	if !strings.Contains(command, "*** ") {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(command, "\n") {
		line = strings.TrimSpace(line)
		for _, header := range applyPatchHeaders {
			rest, ok := strings.CutPrefix(line, header)
			if !ok {
				continue
			}
			if p := strings.TrimSpace(rest); p != "" {
				out = append(out, p)
			}
			break
		}
	}
	return out
}
