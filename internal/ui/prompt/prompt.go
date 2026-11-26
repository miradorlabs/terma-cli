// Package prompt draws a form of checkboxes and radio buttons on a terminal without a
// TUI framework: a pure model driven by decoded keys, and Run, the raw-mode driver.
package prompt

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"
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
		line("")
	}

	// Labels pad to the longest one only when some item has text beside it.
	width, side, sections := 0, false, false
	for _, it := range f.Items {
		width = max(width, utf8.RuneCountInString(it.Label))
		side = side || it.aside() != ""
		sections = sections || it.Heading != ""
	}
	if !side {
		width = 0
	}
	// What precedes the text beside a label: cursor, box unless a choice, label, gap.
	lead := 2 + width + len(detailGap)
	if sections {
		lead += len(sectionIndent)
	}
	if !f.Choice {
		lead += 4
	}
	room := cmp.Or(f.termWidth, fallbackWidth) - 1 - lead

	indent := ""
	for i, it := range f.Items {
		if it.Heading != "" {
			indent = sectionIndent
			if i > 0 {
				line("")
			}
			line("  " + it.Heading)
		}
		here := i == f.cursor && !it.Disabled
		cursor := "  " + indent
		if here {
			cursor = brand("❯") + " " + indent
		}
		label := it.Label
		aside := truncate(it.aside(), room)
		if aside != "" {
			label += strings.Repeat(" ", width-utf8.RuneCountInString(label)) + detailGap
		}
		box := ""
		if !f.Choice {
			box = checkbox(it, brand) + " "
		}
		var text string
		switch {
		case it.Disabled:
			text = dim(cursor + box + label + aside)
		case f.Choice && here:
			text = cursor + brand(label) + dim(aside)
		default:
			text = cursor + box + label + dim(aside)
		}
		line(text)
	}
	return lines
}

// sectionIndent sets the items under a section's heading in from it.
const sectionIndent = "  "

// detailGap separates the label column from the text beside it.
const detailGap = "  "

// fallbackWidth is the width text beside a label is cut to when the terminal's is unknown.
const fallbackWidth = 80

func checkbox(it Item, brand func(string) string) string {
	switch {
	case it.Kind == Radio && it.Selected:
		return brand("(•)")
	case it.Kind == Radio:
		return "( )"
	case it.Selected:
		return brand("[x]")
	}
	return "[ ]"
}

// aside is the text beside an item's label, as plain text on one line: a disabled item's
// Reason, else its Detail.
func (it Item) aside() string {
	s := it.Detail
	if it.Disabled && it.Reason != "" {
		s = it.Reason
	}
	return plain(s)
}

// plain drops escape sequences and control characters, so text from elsewhere, such as a
// team's description, can neither style the frame nor break a line.
func plain(s string) string {
	s = ansiSGR.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// truncate cuts s to n columns, ending in "…" when it had to cut; nothing fits below two.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n < 2 {
		return ""
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

// ansiSGR matches a CSI escape: the colours the palette emits, or any other.
var ansiSGR = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

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
