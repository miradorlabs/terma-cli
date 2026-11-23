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
// past that point that a hook may send. Which of them it sends is captureAntigravityCalls'.

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
	// Before is the transcript's size when this state began: the lines that start before it
	// are never sent and never counted. 0 once a read has passed it, and for a state that
	// began with no transcript.
	Before int64 `json:"before,omitempty"`
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

// antigravityTailLen is how many bytes before the offset identify the file read so far.
const antigravityTailLen = 256

// readAntigravitySteps hands each call, and each invoke_subagent result, past last's
// offset and above last.Step to emit, in order, and returns the state after the last line
// emit took. A line that starts before last.Before is passed without emit. emit returning
// false, or ctx done before a line, stops the read there, for the next hook.
func readAntigravitySteps(ctx context.Context, path string, last antigravityCallState, emit func(s antigravityStep) bool) (antigravityCallState, error) {
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
	if _, err := f.Seek(state.Offset, io.SeekStart); err != nil {
		return last, err
	}
	br := bufio.NewReader(f)
	for {
		if ctx.Err() != nil {
			// Out of time: what was handled is saved, the rest goes with the next hook.
			state.Tail = antigravityTail(f, state.Offset)
			return state, nil
		}
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
			advance(n)
			continue
		}
		if !bytes.Contains(line, []byte(`"PLANNER_RESPONSE"`)) &&
			(len(state.Results) == 0 || !bytes.Contains(line, []byte(`"GENERIC"`))) {
			advance(n)
			continue
		}
		var s antigravityStep
		if json.Unmarshal(line, &s) != nil || s.StepIndex == nil || *s.StepIndex < 0 {
			advance(n)
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
			if state.Offset >= state.Before && !emit(s) {
				state.Tail = antigravityTail(f, state.Offset)
				return state, nil
			}
			state.Step = step
		}
		state.Results = results
		advance(n)
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
