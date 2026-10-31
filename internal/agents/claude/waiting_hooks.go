package claude

import (
	"cmp"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/live"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// A session waits for the developer from a Notification that asks them something until
// their next prompt, the next tool call ending or the turn's end; one still waiting after
// live.AnswerWait nudges them, from a detached `terma notify await-answer`. It is local to
// the machine, so it runs whether or not the team collects the repository.
//
// Claude Code has no hook for the answer itself, and two answers reach none of those hooks:
// a prompt rejected or dismissed ends the turn as an interrupt, with no PostToolUse or Stop,
// but is written to the transcript; and an approved tool has no PostToolUse until it ends,
// but runs as a new child of Claude Code. The wait records the transcript's size and Claude
// Code's children as it begins, and its check skips the nudge on either change
// (live.Wait.answered); anything else the session writes or starts meanwhile counts too, so
// it errs toward silence.
//
// Accepted risk: where procinfo cannot list processes, an approved tool still running two
// minutes after its prompt nudges a developer who already answered; and of two prompts
// waiting at once, the first one answered ends the wait for both. The upgrade path is a hook
// Claude Code may add for the answer.

// claudeWaitingStateDir holds the wait of each session waiting for its developer.
var claudeWaitingStateDir = hookrun.AgentStateDir(name, "waiting")

// waitingNotifications are the notification types that wait on the developer: a permission
// prompt (AskUserQuestion's questions among them) or an MCP server's form or URL. idle_prompt,
// a turn that ended a minute ago, asked nothing, and the others announce.
var waitingNotifications = []string{"permission_prompt", "elicitation_dialog", "elicitation_url_dialog"}

// notification records that the session waits for its developer, and starts the check that
// nudges them, when one is due to be told.
func notification(_ context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) || !slices.Contains(waitingNotifications, in.NotificationType) ||
		env.Notifier == nil || env.AwaitAnswer == nil {
		return nil
	}
	w := live.Wait{ID: rand.Text(), Since: env.Time(), Agent: displayName, Checkout: checkoutName(cmp.Or(in.Cwd, env.Cwd))}
	if info, err := os.Stat(in.TranscriptPath); err == nil && in.TranscriptPath != "" {
		w.Transcript, w.TranscriptSize = in.TranscriptPath, info.Size()
	}
	w.Watch(hookrun.HookCaller(), live.Machine{})
	path := waitPath(env, in.SessionID)
	if err := live.StartWait(path, w); err != nil {
		env.Logf("wait: %v", err)
		return nil
	}
	env.AwaitAnswer(path, w.ID)
	return nil
}

// userPromptSubmit ends the session's wait. It must print nothing: its stdout goes to the
// model; the dispatcher claims the session and starts the relay.
func userPromptSubmit(_ context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	endWait(env, in.SessionID)
	return nil
}

// endWait ends the session's wait: its developer answered, or the turn is over.
func endWait(env hookrun.Env, sessionID string) {
	if session.ValidID(sessionID) {
		live.EndWait(waitPath(env, sessionID))
	}
}

func waitPath(env hookrun.Env, sessionID string) string {
	return filepath.Join(env.StateDir, claudeWaitingStateDir, hookrun.EvidenceID(sessionID)+".json")
}

// checkoutName is the folder of the checkout the session works in, as the developer sees it
// in their terminal: a linked worktree's own, so two sessions of one repository differ.
func checkoutName(cwd string) string {
	if cwd == "" {
		return ""
	}
	if root, _, ok := gitx.LocateFS(cwd); ok {
		return filepath.Base(root)
	}
	return filepath.Base(cwd)
}
