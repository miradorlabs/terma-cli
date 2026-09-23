package hookrun

import (
	"io"
	"strings"
	"testing"
)

// Every reader refuses a payload past the bound by name, whatever the head of it parses
// as. Claude, Codex and OpenCode used to read a truncated payload instead.
func TestEveryHookReaderRefusesOversizedInput(t *testing.T) {
	t.Setenv(antigravityConversationEnv, "")
	pad := strings.Repeat(" ", maxHookInput)
	readers := map[string]struct {
		read    func(io.Reader) error
		payload string
	}{
		"claude":      {func(r io.Reader) error { _, err := readClaudeInput(r); return err }, `{"session_id":"valid"}`},
		"codex":       {func(r io.Reader) error { _, err := readCodexHookInput(r); return err }, `{"session_id":"valid"}`},
		"opencode":    {func(r io.Reader) error { _, err := readOpenCodeInput(r); return err }, `{"session_id":"valid"}`},
		"cursor":      {func(r io.Reader) error { _, err := readCursorInput(r); return err }, `{"conversation_id":"valid"}`},
		"antigravity": {func(r io.Reader) error { _, err := readAntigravityInput(r); return err }, `{"conversationId":"valid"}`},
	}
	for name, c := range readers {
		if err := c.read(strings.NewReader(c.payload)); err != nil {
			t.Errorf("%s: a payload within the bound was refused: %v", name, err)
		}
		err := c.read(strings.NewReader(c.payload + pad))
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Errorf("%s: oversized payload: err = %v, want too large", name, err)
		}
		if err := c.read(nil); err == nil {
			t.Errorf("%s: a nil reader was accepted", name)
		}
	}
}
