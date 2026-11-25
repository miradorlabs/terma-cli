package spinner

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

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

// Updating the text on a running spinner redraws promptly with the new message.
func TestSpinnerUpdatesTextWithoutStopping(t *testing.T) {
	t.Parallel()
	var buf syncBuffer
	s := &Spinner{w: &buf}
	s.Start("Fetching organizations…")
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(buf.String(), "Fetching organizations…") {
		if time.Now().After(deadline) {
			s.Stop()
			t.Fatalf("initial frame not drawn: %q", buf.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	s.Start("Fetching teams…")
	deadline = time.Now().Add(time.Second)
	for !strings.Contains(buf.String(), "Fetching teams…") {
		if time.Now().After(deadline) {
			s.Stop()
			t.Fatalf("updated frame not drawn: %q", buf.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	s.Stop()
	if !strings.HasSuffix(buf.String(), "\r\x1b[2K") {
		t.Fatalf("spinner did not clear line on stop: %q", buf.String())
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
