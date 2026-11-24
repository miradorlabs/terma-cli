package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A batch opens at a call line that asks for tools, and only then are GENERIC lines its
// results. It is tainted by a call line asking for a refused call, or by a line that cannot be
// read; the next call line begins clean. Its calls are judged once, and only when asked.
func TestAntigravityBatch(t *testing.T) {
	refused := antigravityToolCall{Name: "read_file", Args: json.RawMessage(`{"Path":"refused"}`)}
	shown := antigravityToolCall{Name: "read_file", Args: json.RawMessage(`{"Path":"shown"}`)}
	type step func(b *antigravityBatch)
	begin := func(calls ...antigravityToolCall) step { return func(b *antigravityBatch) { b.begin(calls) } }
	unknown := func(b *antigravityBatch) { b.unknown() }
	for _, tc := range []struct {
		name        string
		open, saved bool // as the state was saved
		steps       []step
		wantOpen    bool
		want        bool
		judged      int // calls whose arguments were judged
	}{
		{"nothing read", false, false, nil, false, false, 0},
		{"an open batch carried over", true, false, nil, true, false, 0},
		{"a tainted state carried over", true, true, nil, true, true, 0},
		{"a call line with a refused call", false, false, []step{begin(shown, refused)}, true, true, 2},
		{"a call line with shown calls", true, true, []step{begin(shown, shown)}, true, false, 2},
		{"a call line with no calls", true, true, []step{begin()}, false, false, 0},
		{"an unreadable line", false, false, []step{unknown}, true, true, 0},
		{"a call line after an unreadable line", false, false, []step{unknown, begin(shown)}, true, false, 1},
		{"an unreadable line after a call line", false, false, []step{begin(shown), unknown}, true, true, 0},
		{"a call line with no calls after a refused one", false, false, []step{begin(refused), begin()}, false, false, 0},
		// A batch passed before it is asked about is never judged.
		{"a refused batch passed by the next", false, false, []step{begin(refused), begin(shown)}, true, false, 1},
	} {
		judged := 0
		b := antigravityBatch{open: tc.open, tainted: tc.saved, refuses: func(args json.RawMessage) bool {
			judged++
			return string(args) == `{"Path":"refused"}`
		}}
		for _, s := range tc.steps {
			s(&b)
		}
		if b.open != tc.wantOpen {
			t.Errorf("%s: open %v, want %v", tc.name, b.open, tc.wantOpen)
		}
		if got := b.isTainted(); got != tc.want {
			t.Errorf("%s: tainted %v, want %v", tc.name, got, tc.want)
		}
		if b.isTainted() != tc.want || b.saved() != tc.want {
			t.Errorf("%s: asked again, the answer changed", tc.name)
		}
		if judged != tc.judged {
			t.Errorf("%s: %d arguments judged, want %d", tc.name, judged, tc.judged)
		}
	}
}

// The reader marks a GENERIC line a tool result only inside a batch: not before any call
// line, nor after one that asked for no tool. A batch with more results than calls marks
// them all, and a read that stops inside a batch leaves it open for the next.
func TestAntigravityReaderMarksResultsOnlyInsideABatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), antigravityTranscriptName)
	lines := []string{resultLine(0), replyLine(1), resultLine(2), callLine(3, `{"name":"run_command","args":{}}`),
		resultLine(4), resultLine(5), resultLine(6), callLine(7), resultLine(8)}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	never := func(json.RawMessage) bool { return false }
	results := map[int64]bool{}
	read := func(last antigravityCallState, until int64) antigravityCallState {
		t.Helper()
		next, err := readAntigravitySteps(context.Background(), path, last, -1, never, func(s antigravityStep) bool {
			if *s.StepIndex > until {
				return false
			}
			if s.Type == "GENERIC" {
				results[*s.StepIndex] = s.toolResult
			}
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	// The read stops before step 6, inside the batch of step 3.
	state := read(antigravityCallState{Step: -1}, 5)
	if !state.Batch || state.Tainted {
		t.Fatalf("stopped inside a batch, the state is %+v", state)
	}
	state = read(state, 8)
	if state.Batch {
		t.Errorf("after a call line with no calls, the state is %+v", state)
	}
	want := map[int64]bool{0: false, 2: false, 4: true, 5: true, 6: true, 8: false}
	for step, w := range want {
		if got, read := results[step]; !read || got != w {
			t.Errorf("step %d: read %v, a tool result %v, want %v", step, read, got, w)
		}
	}
}
