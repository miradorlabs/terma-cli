package claude

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// claudeScheduledLoopDir holds each session's live scheduled loops.
var claudeScheduledLoopDir = hookrun.AgentStateDir(name, "scheduled-loops")

// claudeScheduledLoops maps a key for each of the session's live loops, computed and kept on
// this machine, to the random id terma minted for it, the only part that is sent.
type claudeScheduledLoops struct {
	Loops map[string]string `json:"loops"`
}

// userPromptSubmit spools terma.scheduled_loop.iteration for a turn a schedule woke; its caller
// claims the session and starts the relay first. It must print nothing: stdout reaches the model.
func userPromptSubmit(ctx context.Context, env hookrun.Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		return err
	}
	switch in.Source {
	case semconv.TermaScheduledLoopTriggerLoopWakeup, semconv.TermaScheduledLoopTriggerScheduleWakeup:
	default:
		return nil
	}
	if env.Spool == nil || !session.ValidID(in.SessionID) || !session.ValidID(in.PromptID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	env.EmitFor(r, spool.Event{Name: semconv.TermaScheduledLoopIterationEvent, SessionID: in.SessionID, Attrs: map[string]any{
		semconv.GenAIMainAgentNameKey: claudeTool, semconv.TermaEvidenceSourceKey: sourceClaudeHook,
		semconv.TermaTurnIDKey: in.PromptID, semconv.TermaScheduledLoopTriggerKey: in.Source,
		semconv.TermaScheduledLoopIDKey: scheduledLoopID(env, in),
	}})
	return nil
}

// scheduledLoopID is the id of the loop that woke the turn, minted at its first wake-up. A
// session runs one self-paced loop at a time (each ScheduleWakeup supersedes the pending one),
// so that loop is keyed by its trigger alone, whatever prompt it passes itself; a scheduled task
// by its trigger and prompt.
// Accepted risk: two scheduled tasks with one prompt share an id, and a task whose prompt
// changes between fires (an autonomous /loop's first tick carries a preamble its later ones
// do not), a busy lock or lost state each mint another: loops are overcounted, iterations
// never. Moving the key to the task's id needs Claude Code to name it in UserPromptSubmit.
func scheduledLoopID(e hookrun.Env, in *claudeHookInput) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	key := in.Source
	if in.Source == semconv.TermaScheduledLoopTriggerScheduleWakeup {
		key += "\x00" + in.Prompt
	}
	key = hookrun.EvidenceID(key)
	path := scheduledLoopsPath(e.StateDir, in.SessionID)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return id
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return id
	}
	defer unlock()
	var live claudeScheduledLoops
	data, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(data, &live) != nil {
		live = claudeScheduledLoops{}
	}
	if known, ok := live.Loops[key]; ok {
		return known
	}
	if live.Loops == nil {
		live.Loops = map[string]string{}
	}
	live.Loops[key] = id
	if data, err := json.Marshal(live); err == nil {
		if err := hookrun.WriteState(path, data); err != nil {
			e.Logf("scheduled loop state: %v", err)
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(filepath.Dir(path), e.Time().Add(-spool.MaxAge))
	}
	return id
}

// endScheduledLoops forgets the session's loops once Stop says nothing is left to wake it, so
// the next loop gets a new id; a Stop that does not say leaves them.
func endScheduledLoops(e hookrun.Env, in *claudeHookInput) {
	if e.Spool == nil || in.SessionCrons == nil || len(*in.SessionCrons) > 0 || !session.ValidID(in.SessionID) {
		return
	}
	path := scheduledLoopsPath(e.StateDir, in.SessionID)
	if _, err := os.Lstat(path); err != nil {
		return
	}
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		e.Logf("scheduled loop state: %v", err)
	}
}

func scheduledLoopsPath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, claudeScheduledLoopDir, hookrun.EvidenceID(sessionID)+".json")
}
