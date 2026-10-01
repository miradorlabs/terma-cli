package live

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
)

// Terminal drives an interactive harness the way a person does: a real
// pseudo-terminal, keystrokes in, screen output out. Interactive is the mode
// that matters: the status line only runs there, and so do the trust and
// permission dialogs a real session meets.
type Terminal struct {
	cmd  *exec.Cmd
	f    *os.File
	mu   sync.Mutex
	raw  strings.Builder
	done chan error
	// consumed is the raw offset up to which output has been dealt with. A
	// dialog answered once must not keep matching, and a prompt just typed must
	// not pass for the reply, so Expect looks only past this point.
	consumed       int
	screen         vt10x.Terminal
	consumedScreen string
}

// Start spawns command in dir with env on a pty of the given size.
func Start(dir string, env []string, rows, cols uint16, name string, args ...string) (*Terminal, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, err
	}
	t := &Terminal{cmd: cmd, f: f, done: make(chan error, 1), screen: vt10x.New(vt10x.WithSize(int(cols), int(rows)))}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				t.mu.Lock()
				t.raw.Write(buf[:n])
				_, _ = t.screen.Write(buf[:n])
				t.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { t.done <- cmd.Wait() }()
	return t, nil
}

var (
	// cursorRightRE is the one movement worth keeping: a TUI positions words with
	// it instead of spaces, so dropping it would glue words together.
	cursorRightRE = regexp.MustCompile(`\x1b\[(\d*)C`)
	ansiRE        = regexp.MustCompile(`\x1b\[[0-9;?<>=]*[a-zA-Z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][A-Za-z0-9]|\x1b[=>78]|\r`)
)

// Text is everything the program has drawn so far, with escape sequences
// removed and cursor-right movements turned into spaces. Redraws repeat
// content; contracts look for markers, not layout.
func (t *Terminal) Text() string {
	t.mu.Lock()
	raw := t.raw.String()
	if t.screen != nil {
		lines := strings.Split(t.screen.String(), "\n")
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " ")
		}
		raw += "\n" + strings.TrimRight(strings.Join(lines, "\n"), "\n")
	}
	t.mu.Unlock()
	return Strip(raw)
}

// Strip removes terminal escapes from s the way Text does.
func Strip(s string) string {
	s = cursorRightRE.ReplaceAllStringFunc(s, func(m string) string {
		n := 1
		if d := cursorRightRE.FindStringSubmatch(m)[1]; d != "" {
			fmt.Sscanf(d, "%d", &n)
		}
		if n > 200 {
			n = 200
		}
		return strings.Repeat(" ", n)
	})
	return ansiRE.ReplaceAllString(s, "")
}

// TextSince is the stripped output that arrived after the last Consume.
func (t *Terminal) TextSince() string {
	t.mu.Lock()
	raw := t.raw.String()[t.consumed:]
	t.mu.Unlock()
	return Strip(raw)
}

// Consume marks everything drawn so far as dealt with.
func (t *Terminal) Consume() {
	t.mu.Lock()
	t.consumed = t.raw.Len()
	if t.screen != nil {
		t.consumedScreen = t.screen.String()
	}
	t.mu.Unlock()
}

// Raw is the unstripped output, for contracts about colour.
func (t *Terminal) Raw() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.raw.String()
}

// Expect waits until output since the last Consume matches re.
func (t *Terminal) Expect(re *regexp.Regexp, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		if m := t.matchSince(re); m != "" {
			return m, nil
		}
		select {
		case err := <-t.done:
			t.done <- err
			if m := t.matchSince(re); m != "" {
				return m, nil
			}
			return "", fmt.Errorf("program exited before %q appeared (%v)", re, err)
		default:
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for %q", re)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Incremental redraws reuse letters already on screen: stripping CSI commands
// alone can turn TERMA_OK into ERMA_OK. Count screen matches against Consume's
// snapshot so a previous turn's identical response cannot satisfy this turn.
func (t *Terminal) matchSince(re *regexp.Regexp) string {
	if m := re.FindString(t.TextSince()); m != "" {
		return m
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.screen == nil {
		return ""
	}
	before := re.FindAllString(t.consumedScreen, -1)
	after := re.FindAllString(t.screen.String(), -1)
	if len(after) > len(before) {
		return after[len(after)-1]
	}
	return ""
}

// ExpectAny waits for the first of several patterns in output since the last
// Consume and says which one, earlier patterns taking precedence.
func (t *Terminal) ExpectAny(timeout time.Duration, res ...*regexp.Regexp) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		for i, re := range res {
			if t.matchSince(re) != "" {
				return i, nil
			}
		}
		select {
		case err := <-t.done:
			t.done <- err
			return -1, fmt.Errorf("program exited before a prompt appeared (%v)", err)
		default:
		}
		if time.Now().After(deadline) {
			return -1, fmt.Errorf("timed out waiting for any of %d patterns", len(res))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Send types text. A trailing "\r" is Enter.
func (t *Terminal) Send(s string) error {
	_, err := io.WriteString(t.f, s)
	return err
}

// Type sends text slowly enough for a TUI to take it as keystrokes, then Enter.
func (t *Terminal) Type(s string) error {
	for _, r := range s {
		if err := t.Send(string(r)); err != nil {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	return t.Send("\r")
}

// Close asks the program to exit and waits, killing it after the grace period.
func (t *Terminal) Close(exitCommand string, grace time.Duration) error {
	if exitCommand != "" {
		_ = t.Type(exitCommand)
	}
	select {
	case err := <-t.done:
		t.done <- err
		_ = t.f.Close()
		return err
	case <-time.After(grace):
		_ = t.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-t.done:
			t.done <- err
			_ = t.f.Close()
			return err
		case <-time.After(5 * time.Second):
			_ = t.cmd.Process.Kill()
			_ = t.f.Close()
			return fmt.Errorf("killed after %v", grace)
		}
	}
}
