package hookrun

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/desktoprelay"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/session"
)

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
func CodexNotify(ctx context.Context, env Env) error {
	if len(env.Args) == 0 {
		return nil
	}
	payload := env.Args[0]
	defer func() {
		if err := harness.RunPreviousCodexNotify(ctx, payload); err != nil {
			env.logf("previous Codex notify: %v", err)
		}
	}()
	var n codexNotify
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		env.logf("parse codex notify: %v", err)
		return nil
	}
	id := cmp.Or(n.ThreadID, n.TurnID)
	if !session.ValidID(id) {
		return nil
	}
	env.Cwd = cmp.Or(n.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	// notify does not carry transcript_path, but the confined reader can discover
	// the rollout by thread id under CODEX_HOME. Capture before announcing/flushing.
	turn := &codexHookInput{SessionID: id, Cwd: n.Cwd, Model: n.Model, TurnID: n.TurnID}
	env.captureCodexFunding(ctx, r, turn)
	env.captureCodexReplies(ctx, r, turn)
	// Not announce: notify fires at the end of every turn and does not age out manifests.
	sess := env.newSession(r, id, codexTool, n.Model)
	env.setActive(r, sess)
	env.emitStart(r, sess, map[string]any{attrSource: n.Type})
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
// PostToolUse names the files a turn changed, which is what turns a time-window guess
// into a manifest. Both key the session on the same conversation: Codex's hook payload
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
	// ToolUseID matches Codex's native OTLP call_id, so a file touch can join its tool call.
	ToolUseID string `json:"tool_use_id"`
	// ToolInput is whatever the tool was called with; its shape is the tool's own.
	// Codex documents `command` for the shell and apply_patch tools, which is where a
	// file edit is described.
	ToolInput struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	} `json:"tool_input"`
}

func readCodexHookInput(r io.Reader) (*codexHookInput, error) {
	in, err := readHookInput[codexHookInput](r)
	if err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook input has no session_id")
	}
	return in, nil
}

// CodexSessionStart records the Codex session as active.
func CodexSessionStart(ctx context.Context, env Env) error {
	in, err := readCodexHookInput(env.Stdin)
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
	if r.projectID != "" {
		if err := desktoprelay.Register(in.SessionID, r.projectID, env.now()); err != nil {
			env.logf("desktop session registration: %v", err)
		}
	}
	sess := env.newSession(r, in.SessionID, codexTool, in.Model)
	env.setActive(r, sess)
	env.pruneManifests(r, sess.UpdatedAt)
	attrs := map[string]any{attrSource: in.Source}
	// Codex's source dispatches no SessionStart for a thread another thread spawned: the
	// child arrives as the root's SubagentStart, which is where the spawn record is read.
	// That has not been seen live, and this is one line of one file: if a build does
	// start a spawned thread as a session, its start still names its parent.
	if spawn, status := harness.CodexRolloutSpawn(ctx, in.SessionID, in.TranscriptPath); status == statusPresent {
		codexSpawnAttrs(attrs, attrParentSession, spawn)
	}
	env.emitStart(r, sess, attrs)
	return nil
}

// CodexStop drains the thread's quota observations, and what Codex said this turn,
// before starting delivery.
func CodexStop(ctx context.Context, env Env) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	env.captureCodexFunding(ctx, r, in)
	env.captureCodexReplies(ctx, r, in)
	return nil
}

// CodexSessionEnd clears the active session; manifests stay for the commit to come.
//
// Codex ends a session when the conversation is closed, archived or deleted, and
// otherwise after it has been idle and unopened for half an hour — so this can arrive
// long after the work, and never for a session the developer simply leaves open. That
// is why it only clears state: everything a commit needs was already written by the
// time it runs, and a session that never ends is aged out by ActiveTTL instead.
func CodexSessionEnd(ctx context.Context, env Env) error {
	in, err := readCodexHookInput(env.Stdin)
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
	// Capture comes first, as it always has: the end is spooled after the evidence.
	env.captureCodexFunding(ctx, r, in)
	env.captureCodexReplies(ctx, r, in) // whatever a busy Stop left as backlog
	env.endSession(r, in.SessionID, codexTool, in.Reason)
	return nil
}

// CodexPostToolUse adds the files a tool call changed to the session's manifest.
//
// Codex has no file-edit event: edits arrive as tool calls, and the files are named
// inside the patch the call carries. A call that changed nothing — every shell command
// a session runs — leaves no trace here.
func CodexPostToolUse(ctx context.Context, env Env) error {
	in, err := readCodexHookInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	env.captureCodexFunding(ctx, r, in)
	candidates := codexEditedPaths(in)
	if len(candidates) == 0 {
		return nil
	}
	// Reported as a set: the call's own path fields and its patch can name the same file.
	attrs := agentAttrs(map[string]any{}, in.AgentID, in.AgentType)
	boundedAttr(attrs, attrToolCallID, in.ToolUseID)
	env.touch(r, session.Session{ID: in.SessionID, Tool: codexTool, Model: in.Model}, in.ToolName,
		uniqueSorted(relativeFiles(r, env.Cwd, candidates)), attrs)
	return nil
}

// codexEditedPaths returns the files a tool call changed, in the order they appear.
func codexEditedPaths(in *codexHookInput) []string {
	var out []string
	for _, p := range []string{in.ToolInput.FilePath, in.ToolInput.Path} {
		if p != "" {
			out = append(out, p)
		}
	}
	return append(out, applyPatchPaths(in.ToolInput.Command)...)
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
