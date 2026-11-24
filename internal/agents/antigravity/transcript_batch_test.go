package antigravity

import (
	"encoding/json"
	"testing"
)

// A batch is tainted by a call line asking for a refused call, or by a line that cannot be
// read; the next call line begins clean. Its calls are judged once, and only when asked.
func TestAntigravityBatch(t *testing.T) {
	refused := antigravityToolCall{Name: "read_file", Args: json.RawMessage(`{"Path":"refused"}`)}
	shown := antigravityToolCall{Name: "read_file", Args: json.RawMessage(`{"Path":"shown"}`)}
	type step func(b *antigravityBatch)
	begin := func(calls ...antigravityToolCall) step { return func(b *antigravityBatch) { b.begin(calls) } }
	unknown := func(b *antigravityBatch) { b.unknown() }
	for _, tc := range []struct {
		name   string
		saved  bool
		steps  []step
		want   bool
		judged int // calls whose arguments were judged
	}{
		{"nothing read", false, nil, false, 0},
		{"a tainted state carried over", true, nil, true, 0},
		{"a call line with a refused call", false, []step{begin(shown, refused)}, true, 2},
		{"a call line with shown calls", true, []step{begin(shown, shown)}, false, 2},
		{"a call line with no calls", true, []step{begin()}, false, 0},
		{"an unreadable line", false, []step{unknown}, true, 0},
		{"a call line after an unreadable line", false, []step{unknown, begin(shown)}, false, 1},
		{"an unreadable line after a call line", false, []step{begin(shown), unknown}, true, 0},
		// A batch passed before it is asked about is never judged.
		{"a refused batch passed by the next", false, []step{begin(refused), begin(shown)}, false, 1},
	} {
		judged := 0
		b := antigravityBatch{tainted: tc.saved, refuses: func(args json.RawMessage) bool {
			judged++
			return string(args) == `{"Path":"refused"}`
		}}
		for _, s := range tc.steps {
			s(&b)
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
