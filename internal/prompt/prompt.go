// Package prompt draws the one interactive control the CLI needs beyond a yes/no —
// a form of checkboxes and radio buttons — on a terminal, without a TUI framework.
//
// The form is a pure model (Form, handle, render) driven by decoded keypresses, so
// every behaviour is testable with a byte buffer. Run is the thin terminal layer: it
// puts stdin in raw mode, feeds keys to the model, and redraws in place. Anything
// without a terminal never reaches Run — callers check Interactive and fall back to
// flags, which is also how agents and CI use the same commands.
package prompt

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/miradorlabs/terma-cli/internal/style"
)

// Kind is how an Item toggles.
type Kind int

const (
	// Check is an independent on/off switch.
	Check Kind = iota
	// Radio is one of a group: selecting it deselects the others with the same Group.
	Radio
)

// Item is one line of a form.
type Item struct {
	// Label is the short name shown next to the box.
	Label string
	// Detail is the explanation printed after the label, dimmed.
	Detail string
	Kind   Kind
	// Group ties Radio items together; ignored for Check.
	Group    int
	Selected bool
	// Disabled items are drawn but cannot be toggled or landed on. Reason says why,
	// and is printed in place of Detail so the user is told rather than left guessing.
	Disabled bool
	Reason   string
	// Heading, when set, is printed as a section title above this item.
	Heading string
}

// Form is the model: a title, its items, and where the cursor is.
type Form struct {
	Title string
	Items []Item

	cursor int
	// termWidth is the terminal's column count, used so render can count the physical
	// rows a wrapped line occupies rather than one row per logical line — the count the
	// redraw backs up over. Zero means "unknown / assume no wrapping" (the model tests
	// exercise render this way).
	termWidth int
}

// ErrCancelled is returned when the user backs out (Esc, q, or Ctrl-C).
var ErrCancelled = errors.New("cancelled")

// ErrNoTerminal is returned by Run when stdin is not a terminal.
var ErrNoTerminal = errors.New("no terminal to prompt on")

type key int

const (
	keyNone key = iota
	keyUp
	keyDown
	keyToggle
	keyEnter
	keyAll
	keyClear
	keyCancel
)

// Interactive reports whether both ends of a prompt are terminals. stdout being a
// terminal is not enough: `echo y | terma connect claude` has a terminal to draw on
// and nothing to read keys from, and must fall through to the line-based confirm.
func Interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// Run shows the form on the terminal, lets the user edit it, and returns the items in
// their final state. The form stays on screen after Enter so the transcript shows what
// was chosen. It draws on stderr, like every other prompt in this CLI, so stdout stays
// clean for the command's own output.
func Run(f *Form) ([]Item, error) {
	if !Interactive() {
		return nil, ErrNoTerminal
	}
	f.init()

	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("raw terminal: %w", err)
	}
	// Restored before anything else is written, including an error: a terminal left in
	// raw mode swallows the user's next Enter and echoes nothing.
	defer func() { _ = term.Restore(fd, state) }()

	out := os.Stderr
	f.termWidth = terminalWidth()
	lines := f.render(out, style.For(out))

	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return nil, fmt.Errorf("read key: %w", err)
		}
		// One read can carry several keys — a paste, or a terminal that batches — so
		// every token is applied, and the frame is redrawn once at the end.
		var done, cancelled bool
		for _, tok := range tokens(buf[:n]) {
			if done, cancelled = f.handle(decode(tok)); done || cancelled {
				break
			}
		}
		if cancelled {
			fmt.Fprint(out, "\r\n")
			return nil, ErrCancelled
		}
		// Redraw in place: back up over the physical rows the frame occupied, clear from
		// there to the end of the screen (so a resize or a shrunk line leaves no residue),
		// then draw the new frame. `lines` is a physical-row count, so a wrapped line does
		// not smear copies down the screen on every keypress.
		fmt.Fprintf(out, "\x1b[%dA\r\x1b[0J", lines)
		f.termWidth = terminalWidth()
		lines = f.render(out, style.For(out))
		if done {
			fmt.Fprint(out, "\r\n")
			return f.Items, nil
		}
	}
}

// tokens splits raw input into keypresses: a CSI sequence (ESC [ x) is one token, any
// other byte is one on its own. A lone trailing ESC is a token too — Esc means cancel.
func tokens(b []byte) [][]byte {
	var out [][]byte
	for i := 0; i < len(b); {
		if b[i] == 0x1b && i+2 < len(b) && b[i+1] == '[' {
			out = append(out, b[i:i+3])
			i += 3
			continue
		}
		out = append(out, b[i:i+1])
		i++
	}
	return out
}

// init puts the cursor on the first item that can be acted on.
func (f *Form) init() {
	f.cursor = 0
	for i, it := range f.Items {
		if !it.Disabled {
			f.cursor = i
			return
		}
	}
}

// handle applies one keypress and reports whether the form is finished.
func (f *Form) handle(k key) (done, cancelled bool) {
	switch k {
	case keyUp:
		f.move(-1)
	case keyDown:
		f.move(1)
	case keyToggle:
		f.toggle(f.cursor)
	case keyAll:
		for i := range f.Items {
			if f.Items[i].Kind == Check && !f.Items[i].Disabled {
				f.Items[i].Selected = true
			}
		}
	case keyClear:
		for i := range f.Items {
			if f.Items[i].Kind == Check && !f.Items[i].Disabled {
				f.Items[i].Selected = false
			}
		}
	case keyEnter:
		return true, false
	case keyCancel:
		return false, true
	}
	return false, false
}

// move steps the cursor, skipping disabled items and stopping at the ends.
func (f *Form) move(delta int) {
	for i := f.cursor + delta; i >= 0 && i < len(f.Items); i += delta {
		if !f.Items[i].Disabled {
			f.cursor = i
			return
		}
	}
}

func (f *Form) toggle(i int) {
	if i < 0 || i >= len(f.Items) || f.Items[i].Disabled {
		return
	}
	it := &f.Items[i]
	if it.Kind == Check {
		it.Selected = !it.Selected
		return
	}
	// A radio button cannot be unselected: one of the group is always chosen.
	for j := range f.Items {
		if f.Items[j].Kind == Radio && f.Items[j].Group == it.Group {
			f.Items[j].Selected = j == i
		}
	}
}

// decode maps the raw bytes of one keypress onto a key. Arrow keys arrive as CSI
// sequences; a lone escape is a cancel.
func decode(b []byte) key {
	switch {
	case len(b) == 0:
		return keyNone
	case len(b) >= 3 && b[0] == 0x1b && b[1] == '[':
		switch b[2] {
		case 'A':
			return keyUp
		case 'B':
			return keyDown
		}
		return keyNone
	case len(b) == 1:
		switch b[0] {
		case 0x1b, 'q', 0x03, 0x04: // Esc, q, Ctrl-C, Ctrl-D
			return keyCancel
		case 'k':
			return keyUp
		case 'j':
			return keyDown
		case ' ', 'x':
			return keyToggle
		case '\r', '\n':
			return keyEnter
		case 'a':
			return keyAll
		case 'n':
			return keyClear
		}
	}
	return keyNone
}

// clearLine erases the current line before it is redrawn, so a shorter frame never
// leaves the tail of a longer one behind.
const clearLine = "\x1b[2K"

// render draws the whole form and returns the number of lines written, which is what
// the next redraw has to back up over. Lines end in \r\n because the terminal is in
// raw mode, where \n alone does not return the carriage.
func (f *Form) render(w io.Writer, p style.Palette) int {
	dim, bold, brand := p.Dim, p.Bold, p.Brand
	lines := 0
	line := func(s string) {
		fmt.Fprint(w, clearLine+s+"\r\n")
		lines += f.rows(s)
	}

	if f.Title != "" {
		line(bold(f.Title))
		line(dim("  ↑/↓ move   space toggle   a all   n none   enter confirm   esc cancel"))
	}

	width := 0
	for _, it := range f.Items {
		if n := len([]rune(it.Label)); n > width {
			width = n
		}
	}

	for i, it := range f.Items {
		if it.Heading != "" {
			line("")
			line("  " + bold(it.Heading))
		}
		box := "[ ]"
		if it.Kind == Radio {
			box = "( )"
		}
		if it.Selected {
			if it.Kind == Radio {
				box = brand("(•)")
			} else {
				box = brand("[x]")
			}
		}
		cursor := "  "
		if i == f.cursor && !it.Disabled {
			cursor = brand("❯") + " "
		}
		label := it.Label + strings.Repeat(" ", width-len([]rune(it.Label)))
		detail := it.Detail
		if it.Disabled && it.Reason != "" {
			detail = it.Reason
		}
		text := cursor + box + " " + label
		if detail != "" {
			text += "   " + detail
		}
		if it.Disabled {
			text = dim(text)
		} else if detail != "" {
			text = cursor + box + " " + label + "   " + dim(detail)
		}
		line(text)
	}
	return lines
}

// ansiSGR matches the colour escape sequences the palette emits, so a line's visible
// width can be measured without counting them.
var ansiSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

// visibleWidth is the column count a line occupies, ignoring colour escapes.
func visibleWidth(s string) int {
	return utf8.RuneCountInString(ansiSGR.ReplaceAllString(s, ""))
}

// rows is how many physical terminal rows a rendered line occupies once the terminal
// wraps it. With an unknown width it is one row per line, which is what the redraw and
// the model tests assume off a terminal.
func (f *Form) rows(s string) int {
	if f.termWidth <= 0 {
		return 1
	}
	if vis := visibleWidth(s); vis > f.termWidth {
		return (vis + f.termWidth - 1) / f.termWidth
	}
	return 1
}

// terminalWidth reports the drawing terminal's column count, or 0 when it cannot be
// determined (in which case render assumes no wrapping).
func terminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
		return w
	}
	return 0
}
