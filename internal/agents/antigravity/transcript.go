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
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// agy has no export, but its transcript records every model call as a PLANNER_RESPONSE
// line with the call's model and token counts, written before the hooks that follow the
// call run. Each hook reads the lines written since the last one and sends one
// terma.model_call per call, and the highest step handled keeps any from going twice, even
// after compaction rewrites the file.
//
// A call goes only from a hook of its own turn, in the checkout and under the sign-in that
// turn began with: the conversation's turn record (turn.go) must name this hook's
// checkout and lane, the call's step must be at or after the turn's first, and the record
// must still say so right before the send. Every other line a hook reads it passes for good:
// the calls of turns before terma watched the conversation, of a turn begun somewhere else
// or under another sign-in, and the last call of a cancelled turn, after which agy runs no
// hook of that turn. A collecting hook counts the calls it passes as lost (turn_ended),
// except on a conversation's first read, which holds turns from before terma watched; a
// hook the policy does not collect counts nothing. A subagent's link passed the same way is
// not a call and is not counted: the subagent is then sent with no parent.

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
	// Step is the highest step index handled, sent or passed, -1 before any.
	Step int64 `json:"step"`
	// Results names the tools of the last call whose results are still to be read: they
	// follow it in order, one line each.
	Results []string `json:"results,omitempty"`
}

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
// opened, that the transcript recorded since the last read and that belongs to the turn
// this hook saw begin, here and under this sign-in. It reports whether it read the
// transcript, so the hook's observation says usage travels apart. With no repository, a
// hook the team's policy does not collect, it passes everything it reads.
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
	next, err := readAntigravitySteps(transcript, last, func(s antigravityStep) bool {
		if first < 0 || *s.StepIndex < first {
			// Before this turn, or no turn of this hook's: passed for good.
			return pass(s)
		}
		if ctx.Err() != nil {
			// Out of time: what was handled is saved, the rest goes with the next hook.
			return false
		}
		// A later turn's first hook rewrites or removes the record before that turn's first
		// call is written (agy waits for PreInvocation), so a line read while the record
		// still names this turn here cannot be a later turn's.
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
	// A conversation's first read holds the turns before terma watched it: not counted.
	if r != nil && readErr == nil && passed > 0 {
		e.CountLost(r, sendas.LostTurnEnded, passed)
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

// readAntigravitySteps hands each call, and each invoke_subagent result, past last's
// offset and above last.Step to emit, in order, and returns the state after the last line
// emit took. emit returning false stops the read there, for the next hook.
func readAntigravitySteps(path string, last antigravityCallState, emit func(s antigravityStep) bool) (antigravityCallState, error) {
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
		// was handled.
		state.Offset = 0
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
			// Too long to read: what results it may have held are unknown.
			state.Results = nil
			state.Offset += n
			continue
		}
		if !bytes.Contains(line, []byte(`"PLANNER_RESPONSE"`)) &&
			(len(state.Results) == 0 || !bytes.Contains(line, []byte(`"GENERIC"`))) {
			state.Offset += n
			continue
		}
		var s antigravityStep
		if json.Unmarshal(line, &s) != nil || s.StepIndex == nil || *s.StepIndex < 0 {
			state.Offset += n
			continue
		}
		step := *s.StepIndex
		// What the line moves on, kept only once it is read: a line emit refuses is read again.
		// A prompt clears nothing: one that joins a running turn leaves its results pending,
		// and after a cancelled turn the next call resets them.
		results := state.Results
		switch s.Type {
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
		if step > state.Step && (s.Type == "PLANNER_RESPONSE" || s.subagentResult) {
			if !emit(s) {
				state.Tail = antigravityTail(f, state.Offset)
				return state, nil
			}
			state.Step = step
		}
		state.Results = results
		state.Offset += n
	}
}

// antigravityLine reads one whole line, or nil for one longer than antigravityLineLimit,
// and the bytes it took; at the end of the file it returns io.EOF and takes none.
func antigravityLine(br *bufio.Reader) (line []byte, n int64, err error) {
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
