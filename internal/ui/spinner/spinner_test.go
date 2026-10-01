package spinner

import (
	"bytes"
	"testing"
	"time"
)

// Captured output — a test buffer, a pipe, an agent — must never see a frame: the
// spinner is a terminal courtesy, and escape sequences in a transcript are noise.
func TestSpinnerIsInertOffATerminal(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf)
	s.Start("checking")
	s.Update("still checking")
	time.Sleep(2 * interval)
	s.Stop()
	s.Stop() // idempotent
	if buf.Len() != 0 {
		t.Fatalf("wrote %q to a buffer", buf.String())
	}
}

// The mark is four squares lit in turn; on one cell that is the eight quadrant
// glyphs sweeping around the square.
func TestFramesSweepTheFourSquares(t *testing.T) {
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
