// Package spinner animates terma's mark beside a line of text while a command waits;
// anywhere but a terminal a person watches, every method is a no-op.
package spinner

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

// frames is the web app's loading mark on one cell: quadrants lit two at a time, clockwise.
var frames = []string{"▘", "▀", "▝", "▐", "▗", "▄", "▖", "▌"}

const interval = 150 * time.Millisecond

// Spinner animates one line of text in place. Zero value is inert; use New.
type Spinner struct {
	w io.Writer
	p style.Palette

	mu     sync.Mutex
	text   string
	stop   chan struct{}
	done   chan struct{}
	active bool
}

// New returns a spinner that draws on w only when w is a terminal.
func New(w io.Writer) *Spinner {
	if !style.Terminal(w) {
		return &Spinner{}
	}
	return &Spinner{w: w, p: style.For(w)}
}

// Start begins animating beside text, or changes the text of a running spinner.
func (s *Spinner) Start(text string) {
	if s.w == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text = text
	if s.active {
		return
	}
	s.active = true
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go s.run(s.stop, s.done)
}

// Update changes the text beside the mark.
func (s *Spinner) Update(text string) {
	if s.w == nil {
		return
	}
	s.mu.Lock()
	s.text = text
	s.mu.Unlock()
}

// Stop ends the animation and clears its line, returning once the last frame is gone.
func (s *Spinner) Stop() {
	if s.w == nil {
		return
	}
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return
	}
	s.active = false
	close(s.stop)
	done := s.done
	s.mu.Unlock()
	<-done
	fmt.Fprint(s.w, "\r\x1b[2K")
}

func (s *Spinner) run(stop, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	i := 0
	s.draw(i)
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			i++
			s.draw(i)
		}
	}
}

func (s *Spinner) draw(i int) {
	s.mu.Lock()
	text := s.text
	s.mu.Unlock()
	fmt.Fprintf(s.w, "\r\x1b[2K%s %s", s.p.Brand(frames[i%len(frames)]), s.p.Dim(text))
}
