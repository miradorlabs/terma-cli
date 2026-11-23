package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// agy has no export, but its transcript records every model call as a PLANNER_RESPONSE
// line with the call's model and token counts, written before the hooks that follow the
// call run. Each hook reads the lines written since the last one and sends one
// terma.model_call per call. A cancelled turn runs no hook: its calls go with the
// conversation's next one, and the step index each call was sent under keeps any from
// going twice, even after compaction rewrites the file.
//
// A call goes only under the checkout its own turn ran in, as a hook there decided: each
// hook decides its turn, the transcript's latest prompt, collected in its checkout (its
// path, origin and project) or not collected at all. A call of a turn no hook decided (one
// before terma watched the conversation) or decided not collected never leaves, nor does
// one whose turn is in doubt: a later decided prompt that a rewrite dropped. A call decided
// for another checkout, as a cancelled turn leaves for a hook elsewhere, is held, a few at
// a time, for a hook in its own; a rewrite drops what is held. The cursor always moves on.

// sourceAntigravityTranscript is terma.evidence.source for what agy's transcript recorded.
const sourceAntigravityTranscript = "antigravity_transcript"

// antigravityTranscriptName is the transcript that holds the whole of every step; the
// compact transcript.jsonl beside it cuts long fields.
const antigravityTranscriptName = "transcript_full.jsonl"

// antigravityCallDir holds how far each conversation's transcript has been read.
var antigravityCallDir = hookrun.AgentStateDir(Agent{}.Name(), "calls")

// antigravityCallLockWait bounds the wait for another hook of the conversation reading the
// same transcript; past it the calls wait for the next hook.
const antigravityCallLockWait = 3 * time.Second

// antigravitySubagentMarker opens the result of invoke_subagent, which names the child
// conversation the subagent runs in.
const antigravitySubagentMarker = "Created the following subagent:"

// antigravityCallState is how far a conversation's transcript was read.
type antigravityCallState struct {
	// Offset is the end of the last whole line read; Tail hashes the bytes before it, so a
	// file rewritten in place is read again from the start.
	Offset int64  `json:"offset"`
	Tail   string `json:"tail,omitempty"`
	// Step is the highest step index sent, -1 before any.
	Step int64 `json:"step"`
	// Turn is the turn of the last prompt read, for the calls after it.
	Turn string `json:"turn,omitempty"`
	// Results names the tools of the last call whose results are still to be read: they
	// follow it in order, one line each.
	Results []string `json:"results,omitempty"`
	// Turns is each recent turn's decision: the checkout a collecting hook ran it in, or ""
	// where the team's policy collects nothing.
	Turns map[string]string `json:"turns,omitempty"`
	// Held are the lines read for a checkout other than the hook's, oldest first.
	Held []antigravityHeld `json:"held,omitempty"`
}

// antigravityHeld is a line held for a hook in its turn's checkout: where it is in the
// transcript, and what it was.
type antigravityHeld struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Step   int64  `json:"step"`
	Turn   string `json:"turn"`
	// Subagent is set on invoke_subagent's result; else the line is a call.
	Subagent bool `json:"subagent,omitempty"`
}

// antigravityHeldKept bounds the lines held; the oldest go first.
const antigravityHeldKept = 16

// antigravityTurnsKept bounds the decisions a conversation's state keeps, newest first.
const antigravityTurnsKept = 32

// antigravityLineLimit bounds a transcript line terma reads; a longer one (a tool's whole
// output) is skipped unread.
const antigravityLineLimit = 8 << 20

// antigravityStep is the part of a transcript line terma reads.
type antigravityStep struct {
	StepIndex       *int64                `json:"step_index"`
	Type            string                `json:"type"`
	Status          string                `json:"status"`
	CreatedAt       string                `json:"created_at"`
	Model           string                `json:"model"`
	InputTokens     json.RawMessage       `json:"input_tokens"`
	CacheReadTokens json.RawMessage       `json:"cache_read_tokens"`
	OutputTokens    json.RawMessage       `json:"output_tokens"`
	ToolCalls       []antigravityToolCall `json:"tool_calls"`
	// Content is read only on an invoke_subagent result, for the child's id.
	Content string `json:"content"`
	// subagentResult is set on the result of an invoke_subagent call.
	subagentResult bool
}

type antigravityToolCall struct {
	Name string `json:"name"`
}

// antigravityTranscript is the hook's transcriptPath when it is the conversation's own
// transcript in agy's brain folder: the full one, never the compact copy.
func antigravityTranscript(in *antigravityHookInput) string {
	p := in.TranscriptPath
	if p == "" || !filepath.IsAbs(p) {
		return ""
	}
	p = filepath.Clean(p)
	if filepath.Base(p) == "transcript.jsonl" {
		p = filepath.Join(filepath.Dir(p), antigravityTranscriptName)
	}
	if filepath.Base(p) != antigravityTranscriptName {
		return ""
	}
	// <appDataDir>/brain/<conversationId>/.system_generated/logs/transcript_full.jsonl
	logs := filepath.Dir(p)
	generated := filepath.Dir(logs)
	conversation := filepath.Dir(generated)
	if filepath.Base(logs) != "logs" || filepath.Base(generated) != ".system_generated" ||
		filepath.Base(conversation) != in.id() || filepath.Base(filepath.Dir(conversation)) != "brain" {
		return ""
	}
	return p
}

// captureAntigravityCalls sends each model call, and each subagent the conversation
// opened, that the transcript recorded since the last read. It reports whether it read
// the transcript, so the hook's observation says usage travels apart. With no repository,
// a hook the team's policy does not collect, it decides its turn not collected and sends
// nothing; a collected turn's call it reaches waits for a hook in that turn's checkout.
func captureAntigravityCalls(ctx context.Context, e hookrun.Env, r *hookrun.Repo, in *antigravityHookInput) bool {
	transcript := antigravityTranscript(in)
	id := in.id()
	if transcript == "" || (r != nil && e.Spool == nil) || !session.ValidID(id) {
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
	last := antigravityCallState{Step: -1}
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &last) != nil {
		last = antigravityCallState{Step: -1}
	}
	before, _ := json.Marshal(last)
	// The hook's own turn is the latest prompt the transcript holds: decide it first. A
	// rewrite found here drops what was held.
	probe, err := readAntigravitySteps(transcript, last, func(antigravityStep, string, int64, int64) bool { return true })
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			e.Logf("antigravity transcript: %v", err)
		}
		return false
	}
	route := antigravityRoute(r)
	last.Turns = decideAntigravityTurn(last.Turns, probe.Turn, route)
	last.Held = probe.Held
	send := func(s antigravityStep, turn string) bool {
		if s.Type == "PLANNER_RESPONSE" {
			return e.EmitFor(r, antigravityCallEvent(id, s, turn))
		}
		// Only invoke_subagent's result names a child; other output could quote one.
		if child := antigravitySubagent(s.Content); s.subagentResult && child != "" && child != id {
			// The child's own hooks name no parent: its session start, again, does.
			return e.EmitFor(r, spool.Event{Name: semconv.TermaSessionStartEvent, SessionID: child, Attrs: map[string]any{
				semconv.GenAIMainAgentNameKey: antigravityTool, semconv.TermaSessionParentIDKey: id,
			}})
		}
		return true
	}
	if r != nil {
		last.Held = sendAntigravityHeld(ctx, transcript, last.Held, last.Turns, route, send)
	}
	var held []antigravityHeld
	next, err := readAntigravitySteps(transcript, last, func(s antigravityStep, turn string, off, n int64) bool {
		decided, ok := last.Turns[turn]
		switch {
		case !ok || decided == "" || antigravityTurnInDoubt(turn, *s.StepIndex, last.Turns):
			// Not collected, before terma watched, or its turn in doubt: it never leaves.
			return true
		case decided != route:
			// Collected in another checkout: held for a hook there.
			if s.Type == "PLANNER_RESPONSE" || s.subagentResult {
				held = append(held, antigravityHeld{Offset: off, Length: n, Step: *s.StepIndex, Turn: turn, Subagent: s.subagentResult})
			}
			return true
		case ctx.Err() != nil:
			// Out of time: what was sent is saved, the rest goes with the next hook.
			return false
		}
		return send(s, turn)
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		e.Logf("antigravity transcript: %v", err)
	}
	if next.Held = append(next.Held, held...); len(next.Held) > antigravityHeldKept {
		next.Held = next.Held[len(next.Held)-antigravityHeldKept:]
	}
	if b, err := json.Marshal(next); err == nil && !bytes.Equal(b, before) {
		if err := hookrun.WriteState(path, b); err != nil {
			e.Logf("antigravity calls state: %v", err)
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
	return err == nil
}

// antigravityCallEvent is one PLANNER_RESPONSE as a terma.model_call. agy counts input
// without the cache reads, which OpenTelemetry's input count includes, and thinking
// inside output: no reasoning count is sent, and the usage is partial, rather than a zero.
func antigravityCallEvent(id string, s antigravityStep, turn string) spool.Event {
	step := *s.StepIndex
	a := map[string]any{
		semconv.GenAIMainAgentNameKey:  antigravityTool,
		semconv.TermaEvidenceSourceKey: sourceAntigravityTranscript,
		semconv.TermaModelCallIDKey:    "step-" + strconv.FormatInt(step, 10),
		semconv.TermaStepIndexKey:      step,
	}
	hookrun.BoundedAttr(a, semconv.GenAIRequestModelKey, s.Model)
	if hookrun.ShortLabel(turn) {
		a[semconv.TermaTurnIDKey] = turn
	}
	switch s.Status {
	case "DONE":
		a[semconv.TermaOperationStatusKey] = "completed"
	case "ERROR":
		a[semconv.TermaOperationStatusKey] = "error"
	}
	input, inPresent, inOK := hookrun.JSONNumber(s.InputTokens, true)
	cached, cachePresent, cacheOK := hookrun.JSONNumber(s.CacheReadTokens, true)
	output, outPresent, outOK := hookrun.JSONNumber(s.OutputTokens, true)
	switch {
	case (inPresent && !inOK) || (cachePresent && !cacheOK) || (outPresent && !outOK):
		a[semconv.TermaUsageStatusKey] = semconv.TermaUsageStatusInvalid
	case !inPresent && !cachePresent && !outPresent:
		a[semconv.TermaUsageStatusKey] = semconv.TermaUsageStatusUnavailable
	default:
		// Thinking and cache writes are never counted apart.
		a[semconv.TermaUsageStatusKey] = semconv.TermaUsageStatusPartial
		if inPresent {
			// A missing cache count is no cache read: cached is then 0.
			a[semconv.GenAIUsageInputTokensKey] = int64(input + cached)
		}
		if cachePresent {
			a[semconv.GenAIUsageCacheReadInputTokensKey] = int64(cached)
		}
		if outPresent {
			a[semconv.GenAIUsageOutputTokensKey] = int64(output)
		}
	}
	ev := spool.Event{Name: semconv.TermaModelCallEvent, SessionID: id, Attrs: a}
	if t, err := time.Parse(time.RFC3339, s.CreatedAt); err == nil {
		ev.Time = t
	}
	return ev
}

// antigravitySubagent is the child conversation an invoke_subagent result names, or "".
func antigravitySubagent(content string) string {
	_, rest, ok := strings.Cut(content, antigravitySubagentMarker)
	if !ok {
		return ""
	}
	var child struct {
		ConversationID string `json:"conversationId"`
	}
	if json.NewDecoder(strings.NewReader(rest)).Decode(&child) != nil || !session.ValidID(child.ConversationID) {
		return ""
	}
	return child.ConversationID
}

// antigravityTailLen is how many bytes before the offset identify the file read so far.
const antigravityTailLen = 256

// readAntigravitySteps hands each whole line past last's offset that may matter to emit,
// in order, with the turn it belongs to, and returns the state after the last line emit
// took. A call at or below last.Step was sent before and is skipped. emit returning false
// stops the read there, for the next hook.
func readAntigravitySteps(path string, last antigravityCallState, emit func(s antigravityStep, turn string, offset, length int64) bool) (antigravityCallState, error) {
	f, err := os.Open(path)
	if err != nil {
		// Missing too: a subagent's first hooks run before its transcript exists.
		return last, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return last, err
	}
	if !info.Mode().IsRegular() {
		return last, errors.New("transcript is not a regular file")
	}
	state := last
	if state.Offset > info.Size() || antigravityTail(f, state.Offset) != state.Tail {
		// Rewritten (compaction) or replaced: read again from the start; Step keeps what
		// was sent, and Turn the turn until a prompt the rewrite kept names it again.
		state.Offset, state.Held = 0, nil
	}
	if _, err := f.Seek(state.Offset, io.SeekStart); err != nil {
		return last, err
	}
	br := bufio.NewReader(f)
	for {
		line, n, err := antigravityLine(br)
		if err != nil {
			// A line without its newline is still being written.
			if errors.Is(err, io.EOF) {
				err = nil
			}
			state.Tail = antigravityTail(f, state.Offset)
			return state, err
		}
		if line == nil {
			// Too long to read: no call line is; what results it may have held are unknown.
			state.Results = nil
			state.Offset += n
			continue
		}
		if !bytes.Contains(line, []byte(`"PLANNER_RESPONSE"`)) && !bytes.Contains(line, []byte(`"USER_INPUT"`)) &&
			(len(state.Results) == 0 || !bytes.Contains(line, []byte(`"GENERIC"`))) {
			state.Offset += int64(len(line))
			continue
		}
		var s antigravityStep
		if json.Unmarshal(line, &s) != nil || s.StepIndex == nil || *s.StepIndex < 0 {
			state.Offset += int64(len(line))
			continue
		}
		step := *s.StepIndex
		// What the line moves on, kept only once it is read: a line emit refuses is read again.
		results := state.Results
		switch s.Type {
		case "USER_INPUT":
			results = nil
		case "PLANNER_RESPONSE":
			results = nil
			for _, c := range s.ToolCalls {
				results = append(results, c.Name)
			}
		case "GENERIC":
			if len(results) > 0 {
				s.subagentResult = results[0] == "invoke_subagent"
				results = results[1:]
			}
		}
		switch {
		case s.Type == "USER_INPUT":
			// agy's turn starts with the prompt's step counted: initialNumSteps, its turn id.
			// A rewrite that kept only an older prompt never moves it back.
			if step+1 > antigravityTurnNumber(state.Turn) {
				state.Turn = "turn-" + strconv.FormatInt(step+1, 10)
			}
		case step <= state.Step:
			// Sent before a rewrite.
		case s.Type == "PLANNER_RESPONSE" || s.subagentResult:
			if !emit(s, state.Turn, state.Offset, n) {
				state.Tail = antigravityTail(f, state.Offset)
				return state, nil
			}
			state.Step = step
		}
		state.Results = results
		state.Offset += int64(len(line))
	}
}

// antigravityLine reads one whole line, or nil for one longer than antigravityLineLimit,
// and the bytes it took; at the end of the file it returns io.EOF and takes none.
func antigravityLine(br *bufio.Reader) ([]byte, int64, error) {
	var line []byte
	var n int64
	over := false
	for {
		chunk, err := br.ReadSlice('\n')
		n += int64(len(chunk))
		if !over && len(line)+len(chunk) <= antigravityLineLimit {
			line = append(line, chunk...)
		} else {
			over, line = true, nil
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err != nil:
			return nil, 0, err
		case over:
			return nil, n, nil
		}
		return line, n, nil
	}
}

// antigravityTail hashes up to antigravityTailLen bytes of f before offset.
func antigravityTail(f *os.File, offset int64) string {
	if offset <= 0 {
		return ""
	}
	n := min(offset, antigravityTailLen)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset-n); err != nil {
		return "unreadable"
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:8])
}
