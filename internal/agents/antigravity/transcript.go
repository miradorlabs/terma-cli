package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// agy has no export, but its transcript records every model call as a PLANNER_RESPONSE
// line with the call's model and token counts, written before the hooks that follow the
// call run. Each hook reads the lines written since the last one and sends one
// terma.model_call per call it may send (see turn.go for which). The highest step handled
// keeps any call from going twice, even after compaction rewrites the file.

// antigravityTranscriptName is the transcript that holds the whole of every step; the
// compact transcript.jsonl beside it cuts long fields.
const antigravityTranscriptName = "transcript_full.jsonl"

// antigravityCallDir holds how far each conversation's transcript has been read.
var antigravityCallDir = hookrun.AgentStateDir(Agent{}.Name(), "calls")

// antigravityCallLockWait bounds the wait for another hook of the conversation reading the
// same transcript; past it the calls wait for the next hook.
const antigravityCallLockWait = 3 * time.Second

// antigravityTranscript is the hook's transcriptPath when it is the conversation's own
// transcript in agy's brain folder: the full one, never the compact copy.
func antigravityTranscript(in *antigravityHookInput) string {
	transcript, _ := antigravityTranscriptPaths(in)
	return transcript
}

// antigravityAppData is the app data folder of the home in's session runs in,
// <home>/<app data>, read from a transcript path antigravityTranscript accepts; "" without
// one.
func antigravityAppData(in *antigravityHookInput) string {
	_, appData := antigravityTranscriptPaths(in)
	return appData
}

// antigravityTranscriptPaths are antigravityTranscript and antigravityAppData, both "" for a
// transcript path that is not the shape agy writes.
func antigravityTranscriptPaths(in *antigravityHookInput) (transcript, appData string) {
	p := in.TranscriptPath
	if p == "" || !filepath.IsAbs(p) {
		return "", ""
	}
	p = filepath.Clean(p)
	if filepath.Base(p) == "transcript.jsonl" {
		p = filepath.Join(filepath.Dir(p), antigravityTranscriptName)
	}
	if filepath.Base(p) != antigravityTranscriptName {
		return "", ""
	}
	// <appDataDir>/brain/<conversationId>/.system_generated/logs/transcript_full.jsonl
	logs := filepath.Dir(p)
	generated := filepath.Dir(logs)
	conversation := filepath.Dir(generated)
	brain := filepath.Dir(conversation)
	if filepath.Base(logs) != "logs" || filepath.Base(generated) != ".system_generated" ||
		filepath.Base(conversation) != in.id() || filepath.Base(brain) != "brain" {
		return "", ""
	}
	return p, filepath.Dir(brain)
}

// captureAntigravityCalls reads the transcript past the last read and sends each model
// call, and each subagent the conversation opened, that the rule in turn.go lets this hook
// send; it passes the rest. It reports whether it read the transcript, so the hook's
// observation says usage travels apart.
func captureAntigravityCalls(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *antigravityHookInput) bool {
	transcript := antigravityTranscript(in)
	id := in.id()
	if transcript == "" || e.Spool == nil || !session.ValidID(id) {
		return false
	}
	dir := filepath.Join(e.StateDir, antigravityCallDir)
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	path := filepath.Join(dir, hookrun.EvidenceID(id)+".json")
	lockCtx, cancel := context.WithTimeout(ctx, antigravityCallLockWait)
	defer cancel()
	unlock, err := flock.Lock(lockCtx, path+".lock")
	if err != nil {
		e.Logf("antigravity calls: %v", err)
		return false
	}
	defer unlock()
	// A state that begins passes what the transcript already holds (see turn.go).
	last := antigravityCallState{Step: -1}
	b, readErr := os.ReadFile(path)
	began := readErr != nil || json.Unmarshal(b, &last) != nil
	if began {
		last = antigravityCallState{Step: -1}
		if info, err := os.Stat(transcript); err == nil {
			last.Before = info.Size()
		}
	}
	before, _ := json.Marshal(last)
	// The turn this hook saw begin here, and its first step; -1 sends nothing.
	own := antigravityTurnID(e, r, id)
	first := antigravityTurnNumber(own)
	passed := 0
	pass := func(s antigravityStep) bool {
		if s.Type == "PLANNER_RESPONSE" {
			passed++
		}
		return true
	}
	next, err := readAntigravitySteps(ctx, transcript, last, func(s antigravityStep) bool {
		if first < 0 || *s.StepIndex < first {
			// Before this turn, or no turn of this hook's: passed for good.
			return pass(s)
		}
		// The record must still name this turn right before the send. That is enough: agy
		// runs PreInvocation before it calls the model (on 1.5.15, a hook that slept 3 s
		// delayed the call by 3 s), so a later turn's first hook rewrites or removes the
		// record before that turn's first call is written. A line read while the record still
		// names this turn is this turn's, or an earlier one's.
		if antigravityTurnID(e, r, id) != own {
			first = -1
			return pass(s)
		}
		if s.Type == "PLANNER_RESPONSE" {
			return e.EmitFor(r, antigravityCallEvent(id, s, own))
		}
		// Only invoke_subagent's result names a child; other output could quote one.
		if child := antigravitySubagent(s.Content); child != "" && child != id {
			// The child's own hooks name no parent: its session start, again, does.
			return e.EmitFor(r, spool.Event{Name: semconv.TermaSessionStartEvent, SessionID: child, Attrs: map[string]any{
				semconv.GenAIMainAgentNameKey: antigravityTool, semconv.TermaSessionParentIDKey: id,
			}})
		}
		return true
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		e.Logf("antigravity transcript: %v", err)
	}
	if passed > 0 {
		e.CountLost(r, sendas.LostTurnEnded, passed)
	}
	// A state that began is saved even unchanged: a subagent's first hook runs before its
	// transcript exists, and the next must not begin again past the subagent's first call.
	if b, err := json.Marshal(next); err == nil && (began || !bytes.Equal(b, before)) {
		if err := hookrun.WriteState(path, b); err != nil {
			e.Logf("antigravity calls state: %v", err)
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
	return err == nil
}
