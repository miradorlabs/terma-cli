// Package prompt draws a form of checkboxes and radio buttons on a terminal without a
// TUI framework: a pure model driven by decoded keys, and Run, the raw-mode driver.
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

	"github.com/miradorlabs/terma-cli/internal/ui/style"
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
	Label string
	// Detail is printed after the label, dimmed.
	Detail string
	Kind   Kind
	// Group ties Radio items together; ignored for Check.
	Group    int
	Selected bool
	// Disabled items are drawn but cannot be toggled or landed on; Reason replaces Detail.
	Disabled bool
	Reason   string
	// Heading is a section title printed above this item.
	Heading string
}

// Form is the model: a title, its items, and where the cursor is.
type Form struct {
	Title string
	Items []Item
	// Choice makes the form a pick-one list: no boxes, the cursor starts on the Selected item.
	Choice bool

	cursor int
	// termWidth lets render count wrapped physical rows for the redraw; zero assumes no wrapping.
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

// Interactive reports whether both ends of a prompt are terminals: with stdin piped
// there is a terminal to draw on but no keys to read.
func Interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// Run shows the form on stderr, lets the user edit it, and returns the items in their
// final state, leaving the form on screen.
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
	// A terminal left in raw mode swallows the next Enter and echoes nothing.
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
		// One read can carry several keys; apply them all, then redraw once.
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
		// Back up over the frame's physical rows and clear to the end of the screen.
		fmt.Fprintf(out, "\x1b[%dA\r\x1b[0J", lines)
		f.termWidth = terminalWidth()
		lines = f.render(out, style.For(out))
		if done {
			fmt.Fprint(out, "\r\n")
			return f.Items, nil
		}
	}
}

// Choose shows items as a pick-one list and returns the index picked; Enter keeps the Selected one.
func Choose(title string, items []Item) (int, error) {
	f := &Form{Title: title, Items: items, Choice: true}
	chosen, err := Run(f)
	if err != nil {
		return -1, err
	}
	for i, it := range chosen {
		if it.Selected {
			return i, nil
		}
	}
	return -1, ErrCancelled
}

// tokens splits raw input into keypresses: a CSI sequence (ESC [ x) or a single byte.
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

// init puts the cursor on the first enabled item, or a Choice form's selected one.
func (f *Form) init() {
	f.cursor = 0
	if f.Choice {
		for i, it := range f.Items {
			if it.Selected && !it.Disabled {
				f.cursor = i
				return
			}
		}
	}
	for i, it := range f.Items {
		if !it.Disabled {
			f.cursor = i
			return
		}
	}
}

func (f *Form) handle(k key) (done, cancelled bool) {
	switch k {
	case keyUp:
		f.move(-1)
	case keyDown:
		f.move(1)
	case keyToggle:
		if f.Choice {
			return f.handle(keyEnter)
		}
		f.toggle(f.cursor)
	case keyAll:
		if f.Choice {
			break
		}
		for i := range f.Items {
			if f.Items[i].Kind == Check && !f.Items[i].Disabled {
				f.Items[i].Selected = true
			}
		}
	case keyClear:
		if f.Choice {
			break
		}
		for i := range f.Items {
			if f.Items[i].Kind == Check && !f.Items[i].Disabled {
				f.Items[i].Selected = false
			}
		}
	case keyEnter:
		if f.Choice {
			if f.cursor < 0 || f.cursor >= len(f.Items) || f.Items[f.cursor].Disabled {
				return false, false
			}
			for i := range f.Items {
				f.Items[i].Selected = i == f.cursor
			}
		}
		return true, false
	case keyCancel:
		return false, true
	}
	return false, false
}

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
	for j := range f.Items {
		if f.Items[j].Kind == Radio && f.Items[j].Group == it.Group {
			f.Items[j].Selected = j == i
		}
	}
}

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

// clearLine erases a line before it is redrawn, so a shorter frame leaves no tail.
const clearLine = "\x1b[2K"

// render draws the form and returns the physical rows written; raw mode needs \r\n.
func (f *Form) render(w io.Writer, p style.Palette) int {
	dim, bold, brand := p.Dim, p.Bold, p.Brand
	lines := 0
	line := func(s string) {
		fmt.Fprint(w, clearLine+s+"\r\n")
		lines += f.rows(s)
	}

	if f.Title != "" {
		line(bold(f.Title))
		if f.Choice {
			line(dim("  ↑/↓ move   enter select   esc cancel"))
		} else {
			line(dim("  ↑/↓ move   space toggle   a all   n none   enter confirm   esc cancel"))
		}
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
		if f.Choice {
			if i == f.cursor && !it.Disabled {
				label = brand(label)
			}
			text := cursor + label
			switch {
			case it.Disabled:
				text = dim(cursor + label + "   " + it.Reason)
			case it.Detail != "":
				text += "   " + dim(it.Detail)
			}
			line(text)
			continue
		}
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

// ansiSGR matches the colour escapes the palette emits.
var ansiSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

func visibleWidth(s string) int {
	return utf8.RuneCountInString(ansiSGR.ReplaceAllString(s, ""))
}

// rows is how many physical rows a rendered line occupies once wrapped.
func (f *Form) rows(s string) int {
	if f.termWidth <= 0 {
		return 1
	}
	if vis := visibleWidth(s); vis > f.termWidth {
		return (vis + f.termWidth - 1) / f.termWidth
	}
	return 1
}

// terminalWidth is stderr's column count, or 0 when unknown.
func terminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
		return w
	}
	return 0
}
