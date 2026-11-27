//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/history"
)

// readOneLine reads bytes from f until '\n' without buffering ahead, so unread bytes remain
// in the OS pipe for pipe-full tests.
func readOneLine(f *os.File) (string, error) {
	var buf bytes.Buffer
	var b [1]byte
	for {
		n, err := f.Read(b[:])
		if n > 0 {
			buf.WriteByte(b[0])
			if b[0] == '\n' {
				return buf.String(), nil
			}
		}
		if err != nil {
			return buf.String(), err
		}
	}
}

// pipeWritable reports whether the write end of an OS pipe currently has buffer space
// (POLLOUT set), without writing any bytes or changing non-blocking state.
func pipeWritable(f *os.File) (bool, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return false, err
	}
	var writable bool
	var pollErr error
	if err := rc.Control(func(fd uintptr) {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		n, e := unix.Poll(fds, 0)
		for errors.Is(e, unix.EINTR) {
			n, e = unix.Poll(fds, 0)
		}
		pollErr = e
		writable = n > 0 && fds[0].Revents&unix.POLLOUT != 0
	}); err != nil {
		return false, err
	}
	return writable, pollErr
}

// saveFileBackedSignIn writes the fixture's sign-in into user-credentials.json so a child
// terma binary reads it without touching the host system keychain.
func saveFileBackedSignIn(t *testing.T, fx *historyFixture) {
	t.Helper()
	if err := config.UpdateFile(fx.cfgDir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	cred := storedSession(&fakeAuth{srv: fx.srv})
	if err := saveSignIn(fx.cfgDir, cred); err != nil {
		t.Fatal(err)
	}
}

// Interrupting a --json-stream upload with SIGINT ends with a summary whose "interrupted"
// is true (and no error line) and process exit code 130.
func TestHistoryUploadJSONStreamInterruptExit130(t *testing.T) {
	bin := termaBinary(t)
	fx := newHistoryFixture(t)
	saveFileBackedSignIn(t, fx)

	hitLogs := make(chan struct{}, 1)
	releaseLogs := make(chan struct{})
	fx.answer(func(f *historyFixture) {
		f.onLogs = func(string) {
			select {
			case hitLogs <- struct{}{}:
			default:
			}
			<-releaseLogs
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "history", "upload", "--json-stream")
	cmd.Env = append(os.Environ(),
		"TERMA_CONFIG_DIR="+fx.cfgDir,
		"TERMA_STATE_DIR="+fx.stateDir,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-hitLogs:
	case <-time.After(10 * time.Second):
		close(releaseLogs)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("timed out waiting for upload to reach /v1/logs\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		close(releaseLogs)
		t.Fatalf("signal interrupt: %v", err)
	}
	close(releaseLogs)

	waitErr := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 130 {
		t.Fatalf("exit = %v, want exit code 130\nstdout:\n%s\nstderr:\n%s", waitErr, stdout.String(), stderr.String())
	}

	lines := streamLines(t, stdout.String())
	last := lines[len(lines)-1]
	if last["type"] != "summary" {
		t.Fatalf("last line = %v, want summary (types=%s)", last, streamTypes(lines))
	}
	var sum history.Summary
	if err := json.Unmarshal([]byte(summaryOf(t, stdout.String())), &sum); err != nil {
		t.Fatal(err)
	}
	if !sum.Interrupted {
		t.Errorf("summary.Interrupted = false, want true\nstdout:\n%s", stdout.String())
	}
}

// A reader that stops draining stdout must not keep upload.lock held after an interrupt:
// once signalled, the process gives up on blocked stdout writes after its grace period,
// releases the lock, and exits 130.
func TestHistoryUploadJSONStreamBlockedStdoutReleasesLock(t *testing.T) {
	bin := termaBinary(t)
	fx := newHistoryFixture(t)
	saveFileBackedSignIn(t, fx)

	// Create enough sessions so stream lines overflow any OS pipe buffer (~16 KiB on macOS,
	// ~64 KiB on Linux) and block stdout writes while upload.lock is held.
	oldTime := time.Now().UTC().Add(-4 * time.Hour)
	projectDir := filepath.Dir(fx.ccPath)
	for i := range 350 {
		sid := fmt.Sprintf("33333333-3333-3333-3333-%012d", i)
		p := filepath.Join(projectDir, sid+".jsonl")
		content := strings.Join([]string{
			`{"type":"user","uuid":"u1","promptId":"p1","sessionId":"` + sid + `","cwd":` + jsonQuote(fx.repoA) + `,"version":"2.1.301","timestamp":"` + oldTime.Format(time.RFC3339Nano) + `","message":{"role":"user","content":"blocked pipe test prompt"}}`,
			`{"type":"assistant","uuid":"a1","sessionId":"` + sid + `","cwd":` + jsonQuote(fx.repoA) + `,"version":"2.1.301","timestamp":"` + oldTime.Add(time.Second).Format(time.RFC3339Nano) + `","requestId":"req_1","message":{"id":"msg_1","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"blocked pipe test reply"}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
		}, "\n") + "\n"
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, oldTime, oldTime)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "history", "upload", "--json-stream")
	cmd.Env = append(os.Environ(),
		"TERMA_CONFIG_DIR="+fx.cfgDir,
		"TERMA_STATE_DIR="+fx.stateDir,
	)
	cmd.Stdout = pw
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Wait for the child's "start" line (emitted only after upload.lock is acquired), without
	// ever touching upload.lock from the parent.
	type lineRes struct {
		line string
		err  error
	}
	startCh := make(chan lineRes, 1)
	go func() {
		l, err := readOneLine(pr)
		startCh <- lineRes{line: l, err: err}
	}()
	select {
	case r := <-startCh:
		if r.err != nil {
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("read start line: %v (stderr=%q)", r.err, stderr.String())
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(r.line), &obj); err != nil || obj["type"] != "start" {
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("first line = %q (%v), want start", r.line, err)
		}
	case err := <-done:
		t.Fatalf("child exited before emitting start: %v (stderr=%q)", err, stderr.String())
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("timed out waiting for start line (stderr=%q)", stderr.String())
	}

	// Stop reading pr and wait deterministically until the OS pipe buffer is full (POLLOUT clears).
	fillDeadline := time.NewTimer(30 * time.Second)
	defer fillDeadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		writable, pollErr := pipeWritable(pw)
		if pollErr != nil {
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("poll pipe: %v", pollErr)
		}
		if !writable {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("child exited before filling stdout pipe: %v (sent=%d, stderr=%q)", err, fx.receivedBy("team-a"), stderr.String())
		case <-fillDeadline.C:
			_ = cmd.Process.Kill()
			<-done
			t.Fatalf("timed out waiting for stdout pipe to fill (sent=%d, stderr=%q)", fx.receivedBy("team-a"), stderr.String())
		case <-ticker.C:
		}
	}
	// Close parent's write end before signalling so only the child owns it.
	_ = pw.Close()

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal interrupt: %v", err)
	}

	select {
	case waitErr := <-done:
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 130 {
			t.Fatalf("exit = %v, want 130; stderr=%q", waitErr, stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("process hung on unread stdout pipe after SIGINT; stderr=%q", stderr.String())
	}

	// Lock must be released immediately after exit.
	unlock, err := history.LockUpload(fx.stateDir, fx.account, config.DefaultProfile)
	if err != nil {
		t.Fatalf("upload.lock still held after interrupted exit: %v", err)
	}
	unlock()
}

// D4: When HOME and config/state directory variables are unset, `terma history upload --json-stream`
// still emits a JSON error line on stdout and exits 1; text mode prints Error: to stderr and exits 1.
func TestHistoryUploadJSONStreamHomeUnsetSubprocess(t *testing.T) {
	bin := termaBinary(t)
	var cleanEnv []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "TERMA_CONFIG_DIR", "TERMA_STATE_DIR":
			continue
		}
		cleanEnv = append(cleanEnv, kv)
	}

	t.Run("json-stream emits error line and exits 1", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "history", "upload", "--json-stream")
		cmd.Env = cleanEnv
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("exit = %v, want 1\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		lines := streamLines(t, stdout.String())
		if got := streamTypes(lines); got != "error" {
			t.Fatalf("lines = %q, want %q\nstdout:\n%s", got, "error", stdout.String())
		}
		if code, msg := lastError(t, stdout.String()); code != "failed" || msg == "" {
			t.Errorf("error = (%q, %q), want failed with non-empty message", code, msg)
		}
	})

	t.Run("text mode prints Error on stderr and exits 1", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "history", "upload")
		cmd.Env = cleanEnv
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("exit = %v, want 1\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("text stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "Error:") {
			t.Errorf("text stderr = %q, want Error: ...", stderr.String())
		}
	})
}
