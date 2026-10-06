package spinner

import (
	"bytes"
	"testing"
	"time"
)

// Captured output never sees a frame.
func TestSpinnerIsInertOffATerminal(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	s := New(&buf)
	s.Start("checking")
	time.Sleep(2 * interval)
	s.Stop()
	s.Stop() // idempotent
	if buf.Len() != 0 {
		t.Fatalf("wrote %q to a buffer", buf.String())
	}
}

// The mark is the eight quadrant glyphs sweeping around one cell.
func TestFramesSweepTheFourSquares(t *testing.T) {
	t.Parallel()
	if len(frames) != 8 {
		t.Fatalf("want 8 frames, got %d", len(frames))
	}
	seen := map[string]bool{}
	for _, f := range frames {
		if seen[f] {
			t.Fatalf("frame %q repeats", f)
		}
		seen[f] = true
	}
}
