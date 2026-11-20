// Package statusline runs the status line command an agent's settings named before terma
// wrapped it: the same bytes on stdin, the same shell, environment and directory, in its own
// process group on Unix, so a cancellation from the agent reaches every process it started.
package statusline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// pipeDrain bounds the wait for output pipes after the shell exits: a descendant can hold them.
const pipeDrain = 100 * time.Millisecond

// Quote is s as one single-quoted POSIX shell word.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ReadHead reads up to limit bytes; when the input is longer, rest continues after head.
func ReadHead(r io.Reader, limit int) (head []byte, rest io.Reader) {
	if r == nil {
		return nil, nil
	}
	head, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return head, nil
	}
	if len(head) > limit {
		return head, r
	}
	return head, nil
}

// Start runs command with `shell -c` (sh when shell is empty), feeding it head and then rest;
// done receives its exit. It returns nil when the command could not start.
func Start(ctx context.Context, command, shell, cwd string, head []byte, rest io.Reader, stdout, stderr io.Writer) (*exec.Cmd, chan error) {
	if shell == "" {
		shell = "sh"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", command)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	configureProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "terma: status line renderer did not start: %v\n", err)
		return nil, nil
	}
	go func() {
		defer stdin.Close()
		if _, err := stdin.Write(head); err != nil {
			return
		}
		if rest != nil {
			_, _ = io.Copy(stdin, rest)
		}
	}()
	// exited only signals the exit, so the signal forwarder stops without consuming done.
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		err := cmd.Wait()
		if errors.Is(err, exec.ErrWaitDelay) {
			// Descendants kept the pipes open: stop the group too.
			_ = cmd.Cancel()
		}
		done <- err
		close(exited)
	}()

	forwardSignals(cmd, exited)
	return cmd, done
}

// Wait is the exit status of a command Start ran: 128+n for a signal, -1 when unknown.
func Wait(cmd *exec.Cmd, done chan error) int {
	err := <-done
	if err == nil {
		return 0
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		if code := exit.ExitCode(); code >= 0 {
			return code
		}
		return signalExitCode(exit)
	}
	return -1
}
