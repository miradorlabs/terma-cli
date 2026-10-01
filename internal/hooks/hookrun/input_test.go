package hookrun

import (
	"strings"
	"testing"
)

// The reader refuses a payload past the bound by name, whatever the head of it parses
// as, and a missing reader.
func TestReadInputRefusesOversizedInput(t *testing.T) {
	type payload struct {
		SessionID string `json:"session_id"`
	}
	const valid = `{"session_id":"valid"}`
	if _, err := ReadInput[payload](strings.NewReader(valid)); err != nil {
		t.Fatalf("a payload within the bound was refused: %v", err)
	}
	if _, err := ReadInput[payload](strings.NewReader(valid + strings.Repeat(" ", MaxInput))); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized payload: err = %v, want too large", err)
	}
	if _, err := ReadInput[payload](nil); err == nil {
		t.Fatal("a nil reader was accepted")
	}
}
