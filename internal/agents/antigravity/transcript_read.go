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
)

// Reading agy's transcript: how far a conversation's transcript was read, and the lines
// past that point that a hook may send. Which of them it sends is
// (*antigravityConversation).send's.

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
	// Tainted is that the last call line asked for a call whose content is refused
	// (antigravityPaths.refuses), or could not be read: every result until the next call line
	// is withheld (antigravityBatch).
	Tainted bool `json:"tainted,omitempty"`
	// Before is the transcript's size when this state began: the lines that start before it
	// are never sent and never counted, but for the turn's own prompt (readAntigravitySteps).
	// 0 once a read has passed it, and for a state that began with no transcript.
	Before int64 `json:"before,omitempty"`
	// Clock is the last time stamped on an event of the conversation, in Unix milliseconds.
	Clock int64 `json:"clock,omitempty"`
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
	// Content is a prompt's text in agy's envelope, a reply's text, or a tool's result, which
	// for invoke_subagent names the child.
	Content string `json:"content"`
	// subagentResult is set on the result of an invoke_subagent call.
	subagentResult bool
	// withheld is set on a result of a tainted call line (antigravityCallState.Tainted).
	withheld bool
	// prompt is set on the USER_INPUT the turn being read began with.
	prompt bool
}

// antigravityToolCall is one call a PLANNER_RESPONSE line asks for, with the arguments the
// call's PostToolUse carries.
type antigravityToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// antigravityTailLen is how many bytes before the offset identify the file read so far.
const antigravityTailLen = 256

// antigravityKept are the line types the reader hands on: a prompt, a model call (and its
// reply), a tool's result.
var antigravityKept = map[string]bool{"USER_INPUT": true, "PLANNER_RESPONSE": true, "GENERIC": true}

// readAntigravitySteps hands each prompt, call and tool result past last's offset and above
// last.Step to emit, in order, and returns the state after the last line emit took. A line
// that starts before last.Before is passed without emit, but for the USER_INPUT at exactly
// step prompt (-1 for none), the turn's own prompt, written just before the turn's first
// hook: it is handed on marked, before the boundary or not. emit returning false, or ctx
// done before a line, stops the read there, for the next hook.
//
// A call line is on disk before any of its results, and names its calls with the arguments
// their hooks get. When refuses refuses any of them, or the line cannot be read, the line
// taints its batch: every result until the next call line is handed on marked withheld, with
// no pairing of a result to its call. The taint is tracked for every line read, before the
// boundary too, as Results is.
func readAntigravitySteps(ctx context.Context, path string, last antigravityCallState, prompt int64, refuses func(json.RawMessage) bool, emit func(s antigravityStep) bool) (antigravityCallState, error) {
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
		// was handled. A read that had not passed the boundary begins it again: what the file
		// holds now was not read.
		state.Offset = 0
		if state.Before > 0 {
			state.Before = info.Size()
		}
	}
	// advance moves the offset past a line read, and the boundary once it is passed.
	advance := func(n int64) {
		state.Offset += n
		if state.Before > 0 && state.Offset >= state.Before {
			state.Before = 0
		}
	}
	batch := antigravityBatch{tainted: state.Tainted, refuses: refuses}
	stop := func() antigravityCallState {
		state.Tainted = batch.saved()
		state.Tail = antigravityTail(f, state.Offset)
		return state
	}
	if _, err := f.Seek(state.Offset, io.SeekStart); err != nil {
		return last, err
	}
	br := bufio.NewReader(f)
	for {
		if ctx.Err() != nil {
			// Out of time: what was handled is saved, the rest goes with the next hook.
			return stop(), nil
		}
		line, n, err := antigravityLine(br)
		if err != nil {
			// A line without its newline is still being written.
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return stop(), err
		}
		if line == nil {
			// Too long to read: the calls or results it held are unknown, so it taints.
			state.Results = nil
			batch.unknown()
			advance(n)
			continue
		}
		if !bytes.Contains(line, []byte(`"PLANNER_RESPONSE"`)) && !bytes.Contains(line, []byte(`"GENERIC"`)) &&
			!bytes.Contains(line, []byte(`"USER_INPUT"`)) {
			advance(n)
			continue
		}
		var s antigravityStep
		if json.Unmarshal(line, &s) != nil {
			// Unreadable, as one too long is: it may be a call line whose calls are unknown.
			state.Results = nil
			batch.unknown()
			advance(n)
			continue
		}
		if s.StepIndex == nil || *s.StepIndex < 0 {
			if s.Type == "PLANNER_RESPONSE" {
				// A call line with no step to place it: its calls are unknown, as an unreadable
				// line's are, so it taints until the next call line.
				state.Results = nil
				batch.unknown()
			}
			advance(n)
			continue
		}
		step := *s.StepIndex
		// What the line moves on, kept only once it is read: a line emit refuses is read again.
		// A prompt clears nothing: one that joins a running turn leaves its results pending,
		// and after a cancelled turn the next call resets them.
		results := state.Results
		planner := s.Type == "PLANNER_RESPONSE"
		switch {
		case planner:
			results = nil
			for _, c := range s.ToolCalls {
				results = append(results, c.Name)
			}
		case s.Type == "GENERIC":
			if len(results) > 0 {
				s.subagentResult = results[0] == "invoke_subagent"
				results = results[1:]
			}
		}
		if step > state.Step && antigravityKept[s.Type] {
			s.prompt = s.Type == "USER_INPUT" && prompt >= 0 && step == prompt
			if state.Offset >= state.Before || s.prompt {
				s.withheld = s.Type == "GENERIC" && batch.isTainted()
				if !emit(s) {
					return stop(), nil
				}
			}
			state.Step = step
		}
		state.Results = results
		if planner {
			batch.begin(s.ToolCalls)
		}
		advance(n)
	}
}

// antigravityBatch is the taint of the batch being read: the results that follow the last
// call line, withheld when it asked for a refused call or could not be read. Whether a call is
// refused is worked out only when a result of the batch is handed on or the read stops, and
// once, so a state that begins does not judge every path the transcript ever named.
type antigravityBatch struct {
	// tainted is the batch's taint once judged, or as the state was saved.
	tainted bool
	// pending are the calls of a call line not judged yet; nil once judged.
	pending []antigravityToolCall
	refuses func(json.RawMessage) bool
}

// begin is a call line asking for calls: a batch of its own, judged later. One that asks for
// none is not tainted.
func (b *antigravityBatch) begin(calls []antigravityToolCall) {
	b.tainted, b.pending = false, nil
	if len(calls) > 0 {
		b.pending = calls
	}
}

// unknown is a line that cannot be read, or a call line with no step to place it: it may have
// asked for calls terma cannot see, so the batch is tainted until the next call line.
func (b *antigravityBatch) unknown() { b.tainted, b.pending = true, nil }

// isTainted reports whether the batch's results are withheld, judging its calls the first time.
func (b *antigravityBatch) isTainted() bool {
	if b.pending != nil {
		b.tainted = antigravityCallsRefused(b.pending, b.refuses)
		b.pending = nil
	}
	return b.tainted
}

// saved is the taint a read that stops keeps in the state (antigravityCallState.Tainted), for
// the next hook's read to begin from.
func (b *antigravityBatch) saved() bool { return b.isTainted() }

// antigravityCallsRefused reports whether refuses refuses the arguments of any of calls, or
// one has arguments the reader cannot judge: none at all, or not a JSON object. {} and null
// name no file.
func antigravityCallsRefused(calls []antigravityToolCall, refuses func(json.RawMessage) bool) bool {
	if refuses == nil {
		return false
	}
	for _, c := range calls {
		var args map[string]json.RawMessage
		if len(c.Args) == 0 || json.Unmarshal(c.Args, &args) != nil || refuses(c.Args) {
			return true
		}
	}
	return false
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
