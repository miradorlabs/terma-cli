package codex

import (
	"context"
	"io"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// endless is a payload whose writer never closes it.
type endless struct{ n int64 }

func (e *endless) Read(p []byte) (int, error) { e.n += int64(len(p)); return len(p), nil }

// UserPromptSubmit reads no more of a runaway payload than any hook would.
func TestCodexPromptHookBoundsItsRead(t *testing.T) {
	src := &endless{}
	if err := userPromptSubmit(context.Background(), hookrun.Env{Stdin: io.Reader(src)}); err != nil {
		t.Fatal(err)
	}
	if src.n > hookrun.MaxInput+1 {
		t.Errorf("read %d bytes, want at most MaxInput+1", src.n)
	}
}
