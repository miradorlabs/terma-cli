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

type shade int

const (
	shadeBright shade = iota
	shadeBrand
	shadeMuted
)

type frame struct {
	left, right           string
	leftShade, rightShade shade
}

// frames animates Terma's 2x2 mark across two half-block cells (~1:1 square footprint),
// starting on the signature diagonal (▀▄) and sweeping clockwise through bright, brand,
// and muted violet tones so the mark stays crisp on both dark and light terminals.
var frames = []frame{
	{"▀", "▄", shadeBright, shadeBrand},
	{"▀", "▀", shadeBrand, shadeBright},
	{"▀", "▀", shadeMuted, shadeBrand},
	{"▄", "▀", shadeMuted, shadeBright},
	{"▄", "▀", shadeBright, shadeBrand},
	{"▄", "▄", shadeBrand, shadeBright},
	{"▄", "▄", shadeMuted, shadeBrand},
	{"▀", "▄", shadeMuted, shadeBright},
}

const interval = 100 * time.Millisecond

// Spinner animates one line of text in place. Zero value is inert; use New.
type Spinner struct {
	w io.Writer
	p style.Palette

	mu     sync.Mutex
	text   string
	stop   chan struct{}
	done   chan struct{}
	redraw chan struct{}
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
		select {
		case s.redraw <- struct{}{}:
		default:
		}
		return
	}
	s.active = true
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.redraw = make(chan struct{}, 1)
	go s.run(s.stop, s.done, s.redraw)
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

func (s *Spinner) run(stop, done, redraw chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	i := 0
	s.draw(i)
	for {
		select {
		case <-stop:
			return
		case <-redraw:
			s.draw(i)
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
	f := frames[i%len(frames)]
	mark := s.paint(f.left, f.leftShade) + s.paint(f.right, f.rightShade)
	fmt.Fprintf(s.w, "\r\x1b[2K  %s  %s", mark, s.p.Dim(text))
}

func (s *Spinner) paint(glyph string, sh shade) string {
	switch sh {
	case shadeBright:
		return s.p.BrandBright(glyph)
	case shadeMuted:
		return s.p.BrandMuted(glyph)
	default:
		return s.p.Brand(glyph)
	}
}
