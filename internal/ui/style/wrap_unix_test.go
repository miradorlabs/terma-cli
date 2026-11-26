//go:build darwin || linux

package style

import (
	"testing"

	"golang.org/x/sys/unix"
)

// An agent or a dumb terminal reads the text whole, so even a real terminal is not wrapped.
func TestColumnsIsZeroForAgentsAndDumbTerminals(t *testing.T) {
	tty, master, err := openPTY()
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	defer master.Close()
	defer tty.Close()
	if err := unix.IoctlSetWinsize(int(tty.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 120, Row: 40}); err != nil {
		t.Skipf("cannot size the pseudo-terminal: %v", err)
	}
	for _, key := range agentEnvVars {
		t.Setenv(key, "")
	}
	t.Setenv("TERM", "xterm-256color")
	if got := Columns(tty); got != 120 {
		t.Fatalf("Columns = %d on a 120-column terminal", got)
	}
	t.Setenv("CLAUDECODE", "1")
	if got := Columns(tty); got != 0 {
		t.Fatalf("Columns = %d for an agent, want 0", got)
	}
	t.Setenv("CLAUDECODE", "")
	t.Setenv("TERM", "dumb")
	if got := Columns(tty); got != 0 {
		t.Fatalf("Columns = %d for TERM=dumb, want 0", got)
	}
}
