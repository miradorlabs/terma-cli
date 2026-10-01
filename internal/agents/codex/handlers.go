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
	"github.com/miradorlabs/terma-cli/internal/routing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// State directories under the hook state directory: how far into a rollout the quota,
// reply and Desktop captures have read, when each thread's name was last spooled, and
// the tool calls a PreToolUse started.
const (
	codexFundingCursorDir = "funding-cursors"
	codexReplyCursorDir   = "reply-cursors"
	codexDesktopCursorDir = "desktop-cursors"
	codexTitleStateDir    = "codex-titles"
	codexToolStartDir     = "codex-tool-starts"
)

// Values of evidence_source: the rollout, a hook payload, and
// $CODEX_HOME/session_index.jsonl, where Codex names threads.
const (
	sourceCodexRollout      = "codex_rollout"
	sourceCodexHook         = "codex_hook"
	sourceCodexSessionIndex = "codex_session_index"
)

// codexCaptureTimeout bounds each of a hook's rollout captures, quota and then replies.
// Stop is synchronous with a three-second timeout in the committed hooks file, and both
// captures have to finish inside it.
const codexCaptureTimeout = time.Second

// --- Codex notify (user scope) --------------------------------------------------------

// codexNotify is the JSON Codex passes as the single argument to its `notify`
// program at the end of each turn. Codex reports no per-file edits, so it only
// ever attributes through the active-session fallback.
type codexNotify struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread-id"`
	TurnID   string `json:"turn-id"`
	Cwd      string `json:"cwd"`
	Model    string `json:"model"`
}

const codexTool = "codex"

// CodexNotify marks the Codex thread as active and drains its rollout quota.
// Unlike project hooks, notify is user-scope and is therefore the one funding
// capture path setup can guarantee for every repository.
func CodexNotify(ctx context.Context, env hookrun.Env) error {
	if len(env.Args) == 0 {
		return nil
	}
	payload := env.Args[0]
	defer func() {
		if err := RunPreviousCodexNotify(ctx, payload); err != nil {
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
	// notify does not carry transcript_path, but the confined reader can discover
	// the rollout by thread id under CODEX_HOME. Capture before announcing/flushing.
	turn := &codexHookInput{SessionID: id, Cwd: n.Cwd, Model: n.Model, TurnID: n.TurnID}
	captureCodexFunding(env, ctx, r, turn)
	captureCodexDesktopActivity(env, ctx, r, turn)
	captureCodexReplies(env, ctx, r, turn)
	captureCodexTitle(env, ctx, r, turn)
	// Not announce: notify fires at the end of every turn and does not age out manifests.
	sess := env.NewSession(r, id, codexTool, n.Model)
	env.SetActive(r, sess)
	env.EmitStart(r, sess, map[string]any{hookrun.AttrSource: n.Type})
	return nil
}

// --- Codex project hooks --------------------------------------------------------------

// Codex reaches terma two ways, and they are not alternatives.
//
// `notify` is user-scope, written by a machine-wide `terma connect codex`. It fires once at the end of a turn
// and reports no per-file edits, so a repository wired only that way attributes commits
// through the active-session fallback.
//
// The project hooks below are repo-scope, written by `terma install` and committed.
// PostToolUse names local tool calls and the files a turn changed. Both key the session on the same conversation: Codex's hook payload
// carries `session_id` where the notify payload spells it `thread-id`, and both are the
// thread the turn belongs to, so a repository with hooks *and* notify records one
// session, not two.
//
// The field names are Codex's published stdin schema (codex-rs/hooks/schema/generated).
// Only what terma reads is declared.
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
	// ToolUseID matches Codex's native OTLP call_id, so a file touch can join its tool call.
	ToolUseID string `json:"tool_use_id"`
	// ToolInput is whatever the tool was called with; its shape is the tool's own.
	// Codex documents `command` for the shell and apply_patch tools, which is where a
	// file edit is described.
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

const codexDesktopSurface = "desktop"

// codexDesktopRoute is the repository-local opt-in for desktop capture.
func codexDesktopRoute(r *hookrun.Repo) (routing.Record, bool) {
	if r.ProjectID == "" {
		return routing.Record{}, false
	}
	rec, ok, err := routing.LoadRecord(r.ProjectID)
	return rec, err == nil && ok && slices.Contains(rec.Surfaces, desktop) &&
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

// CodexSessionStart records the Codex session as active.
func CodexSessionStart(ctx context.Context, env hookrun.Env) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		env.Logf("ignoring unsafe session id")
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		env.Logf("not in a git repository: %v", err)
		return nil
	}
	sess := env.NewSession(r, in.SessionID, codexTool, in.Model)
	env.SetActive(r, sess)
	env.PruneManifests(r, sess.UpdatedAt)
	attrs := map[string]any{hookrun.AttrSource: in.Source}
	if _, desktop := codexDesktopRoute(r); desktop {
		attrs["capture_surface"] = codexDesktopSurface
		if dir, err := config.Dir(); err == nil {
			hookrun.PruneState(filepath.Join(dir, codexToolStartDir), env.Time().Add(-spool.MaxAge))
		}
	}
	// Codex's source dispatches no SessionStart for a thread another thread spawned: the
	// child arrives as the root's SubagentStart, which is where the spawn record is read.
	// That has not been seen live, and this is one line of one file: if a build does
	// start a spawned thread as a session, its start still names its parent.
	if spawn, status := CodexRolloutSpawn(ctx, in.SessionID, in.TranscriptPath); status == hookrun.StatusPresent {
		codexSpawnAttrs(attrs, hookrun.AttrParentSession, spawn)
	}
	env.EmitStart(r, sess, attrs)
	return nil
}

// CodexUserPromptSubmit records a desktop turn from the trusted repository hook.
// The prompt travels only when this repository opted into prompt content.
func CodexUserPromptSubmit(ctx context.Context, env hookrun.Env) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil || !session.ValidID(in.SessionID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	route, desktop := codexDesktopRoute(r)
	if !desktop || in.TurnID == "" {
		return nil
	}
	attrs := hookrun.EvidenceAttrs(codexTool, sourceCodexHook, "UserPromptSubmit")
	attrs["capture_surface"] = codexDesktopSurface
	attrs["prompt_bytes"] = len(in.Prompt)
	hookrun.BoundedAttr(attrs, hookrun.AttrTurnID, in.TurnID)
	hookrun.BoundedAttr(attrs, hookrun.AttrModel, in.Model)
	if route.IncludePrompts {
		attrs["prompt"] = boundedCodexContent(in.Prompt)
	}
	env.EmitFor(r, spool.Event{Name: hookrun.EventUserPrompt, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
	return nil
}

// CodexStop drains the thread's quota observations, what Codex said this turn and the
// name it gave the thread, before starting delivery.
func CodexStop(ctx context.Context, env hookrun.Env) error {
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
	captureCodexFunding(env, ctx, r, in)
	captureCodexDesktopActivity(env, ctx, r, in)
	captureCodexReplies(env, ctx, r, in)
	captureCodexTitle(env, ctx, r, in)
	return nil
}

// CodexSessionEnd clears the active session; manifests stay for the commit to come.
//
// Codex ends a session when the conversation is closed, archived or deleted, and
// otherwise after it has been idle and unopened for half an hour — so this can arrive
// long after the work, and never for a session the developer simply leaves open. That
// is why it only clears state: everything a commit needs was already written by the
// time it runs, and a session that never ends is aged out by ActiveTTL instead.
func CodexSessionEnd(ctx context.Context, env hookrun.Env) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		env.Logf("ignoring unsafe session id")
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	// Capture comes first, as it always has: the end is spooled after the evidence.
	captureCodexFunding(env, ctx, r, in)
	captureCodexDesktopActivity(env, ctx, r, in)
	captureCodexReplies(env, ctx, r, in) // whatever a busy Stop left as backlog
	captureCodexTitle(env, ctx, r, in)
	env.EndSession(r, in.SessionID, codexTool, in.Reason)
	return nil
}

// CodexPostToolUse records a Desktop tool call and adds edited files to its manifest.
//
// Codex has no file-edit event: edits arrive as tool calls, and the files are named
// inside the patch the call carries. A call that changed nothing — every shell command
// a session runs — leaves no file-touch record.
func CodexPostToolUse(ctx context.Context, env hookrun.Env) error {
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
	captureCodexFunding(env, ctx, r, in)
	if route, desktop := codexDesktopRoute(r); desktop && in.ToolName != "" {
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
		if route.IncludeToolContent {
			attrs["arguments"] = boundedCodexContent(string(in.ToolInput))
			attrs["output"] = boundedCodexContent(string(in.ToolResponse))
		}
		if success, known := codexToolSuccess(in.ToolResponse); known {
			if success {
				attrs[hookrun.AttrStatus] = "completed"
			} else {
				attrs[hookrun.AttrStatus] = "error"
			}
		}
		env.EmitFor(r, spool.Event{Name: hookrun.EventToolCall, SessionID: in.SessionID, Repo: r.Name, Attrs: attrs})
	}
	captureCodexDesktopActivity(env, ctx, r, in)
	candidates := codexEditedPaths(in)
	if len(candidates) == 0 {
		return nil
	}
	// Reported as a set: the call's own path fields and its patch can name the same file.
	attrs := hookrun.AgentAttrs(map[string]any{}, in.AgentID, in.AgentType)
	hookrun.BoundedAttr(attrs, hookrun.AttrToolCallID, in.ToolUseID)
	if _, desktop := codexDesktopRoute(r); desktop {
		attrs["capture_surface"] = codexDesktopSurface
	}
	env.Touch(r, session.Session{ID: in.SessionID, Tool: codexTool, Model: in.Model}, in.ToolName,
		hookrun.UniqueSorted(hookrun.RelativeFiles(r, env.Cwd, candidates)), attrs)
	return nil
}

// codexEditedPaths returns the files a tool call changed, in the order they appear.
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

// applyPatchHeaders are the lines of Codex's apply_patch envelope that name a file.
// "Move to" names the destination of a rename, whose source is on the Update line
// above it, so both are recorded: the commit touches both paths.
var applyPatchHeaders = []string{
	"*** Add File:",
	"*** Update File:",
	"*** Delete File:",
	"*** Move to:",
}

// applyPatchPaths pulls the file paths out of an apply_patch envelope.
//
// The envelope is read out of the raw command text rather than a structured field
// because Codex has none: an edit is a tool call whose `command` carries the patch,
// whether the tool is named apply_patch or the patch is heredoc'd into a shell call.
// Scanning the text catches both, and a command with no envelope yields nothing.
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
